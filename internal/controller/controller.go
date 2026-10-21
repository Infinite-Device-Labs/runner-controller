// Package controller decides when to start and stop runners, and for which
// scale set, within one memory budget. systemd runs them; the controller
// starts and stops their units, writes each one's JIT config file, and cleans
// up after them.
package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
	"github.com/Infinite-Device-Labs/runner-controller/internal/systemd"
)

// Controller runs the scale sets of one configuration.
type Controller struct {
	log      *slog.Logger
	sd       systemd.Manager
	budget   int64
	stateDir string
	pools    map[string]*config.Pool
	sets     []*scaleSet
	byName   map[string]*scaleSet

	// owner names the session's holder and prefixes runner names.
	owner string
	// now and suffix are replaced in tests.
	now    func() time.Time
	suffix func() string
	// tick is the reconcile interval when nothing else wakes the loop.
	tick time.Duration
	// sessionRetry is the first wait before a failed session is reopened.
	sessionRetry time.Duration
	// emptyPollWait is the minimum interval between empty polls, in case
	// GitHub stops holding them open.
	emptyPollWait time.Duration
	// sleep waits for d or until ctx ends; replaced in tests.
	sleep func(ctx context.Context, d time.Duration)

	mu sync.Mutex
	// runners are the live units, by unit name; the budget counts them.
	runners  map[string]*runner
	removals []*removal
	// blocked is the scale set the budget last held back, logged once.
	blocked *scaleSet
	// seq numbers state snapshots taken by save.
	seq uint64

	// saveMu serialises state file writes; written is the seq on disk.
	saveMu  sync.Mutex
	written uint64

	starts sync.WaitGroup
	wake   chan struct{}
}

// scaleSet is one configured scale set and what is known of its demand.
type scaleSet struct {
	cfg    *config.ScaleSet
	pool   *config.Pool
	client actions.Client
	// index is its place in the file, the last tie-break between scale sets.
	index int
	id    int

	// The rest is guarded by Controller.mu.

	// assigned is GitHub's count of jobs assigned, waiting or running.
	assigned int
	// pending maps assigned, not yet started jobs to their queue times.
	// Messages carry at most 50 events, so it can miss jobs: it only orders
	// scale sets, while assigned decides how many runners.
	pending map[int64]time.Time
	// demandSince is when the scale set last started needing a runner.
	demandSince time.Time
	// failures counts consecutive runners that failed to start or exited
	// without a job; retryAt is when the next start may happen.
	failures int
	retryAt  time.Time
}

// runner is a unit the controller started, or found running at startup.
type runner struct {
	record
	unit string
	// started: systemctl start has returned. Until then the unit isn't
	// polled for exit, though its memory counts.
	started bool
	// startedAt: when systemctl start returned, or when the unit was adopted.
	startedAt time.Time
	// busy: seen running a job. done: that job completed, so the runner is
	// about to exit and no longer counts towards its scale set.
	busy bool
	done bool
	// adopted: found running at startup, job unknown. Counts as busy until
	// GitHub says otherwise.
	adopted bool
	// foreign: a running instance of a pool's template with no record.
	// Counts against the budget until it exits; otherwise left alone.
	foreign bool

	// Scale-down progress: stopping while the registration is being removed,
	// deregistered once it is, stopRequested once systemd accepted the stop.
	// A deregistered runner can't take a job, so it no longer counts towards
	// its scale set, but its memory does until the unit exits. nextScaleDown
	// delays a retry after a failed attempt.
	stopping      bool
	deregistered  bool
	stopRequested bool
	nextScaleDown time.Time
}

// New returns a Controller for cfg, which must have been checked. clients
// holds each scale set's Client, by the scale set's name.
func New(cfg *config.Config, clients map[string]actions.Client, sd systemd.Manager, log *slog.Logger) (*Controller, error) {
	owner, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("reading the hostname: %w", err)
	}
	c := &Controller{
		log:           log,
		sd:            sd,
		budget:        int64(cfg.MemoryBudget),
		stateDir:      cfg.StateDir,
		pools:         cfg.Pools,
		byName:        map[string]*scaleSet{},
		owner:         owner,
		now:           time.Now,
		suffix:        randomSuffix,
		tick:          5 * time.Second,
		sessionRetry:  5 * time.Second,
		emptyPollWait: 2 * time.Second,
		sleep:         sleep,
		runners:       map[string]*runner{},
		wake:          make(chan struct{}, 1),
	}
	for i, s := range cfg.ScaleSets {
		client := clients[s.Name]
		if client == nil {
			return nil, fmt.Errorf("no client for scale set %q", s.Name)
		}
		set := &scaleSet{cfg: s, pool: cfg.Pools[s.Pool], client: client, index: i, pending: map[int64]time.Time{}}
		c.sets = append(c.sets, set)
		c.byName[s.Name] = set
	}
	return c, nil
}

// Run ensures the scale sets exist, recovers from an earlier run, then
// manages runners until ctx ends. Running units are left to finish their
// jobs; the next run adopts them.
func (c *Controller) Run(ctx context.Context) error {
	for _, s := range c.sets {
		id, change, err := s.client.EnsureScaleSet(ctx, actions.ScaleSetSpec{
			Name:        s.cfg.Name,
			RunnerGroup: s.cfg.RunnerGroup,
			Labels:      s.cfg.Labels,
		})
		if err != nil {
			return err
		}
		s.id = id
		c.log.Info("scale set ready", "scale_set", s.cfg.Name, "id", id, "change", change, "pool", s.pool.Name)
	}
	if err := prepareDirs(c.stateDir, c.pools); err != nil {
		return err
	}
	if err := c.recover(ctx); err != nil {
		return err
	}

	var sessions sync.WaitGroup
	for _, s := range c.sets {
		sessions.Go(func() { c.listen(ctx, s) })
	}
	c.loop(ctx)
	sessions.Wait()
	c.starts.Wait()
	return nil
}

func (c *Controller) loop(ctx context.Context) {
	t := time.NewTicker(c.tick)
	defer t.Stop()
	for {
		c.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-t.C:
		}
	}
}

// signal asks the loop to reconcile soon.
func (c *Controller) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// reconcile releases exited units, retries pending registration removals,
// stops surplus idle runners, and starts what the budget allows.
func (c *Controller) reconcile(ctx context.Context) {
	c.checkUnits(ctx)
	c.removeRegistrations(ctx)
	c.scaleDown(ctx)
	c.schedule(ctx)
}

func (c *Controller) checkUnits(ctx context.Context) {
	c.mu.Lock()
	var units []string
	for unit, r := range c.runners {
		if r.started {
			units = append(units, unit)
		}
	}
	c.mu.Unlock()
	if len(units) == 0 {
		return
	}
	states, err := c.sd.ActiveStates(ctx, units)
	if err != nil {
		c.log.Warn("couldn't read the units' states", "error", err)
		return
	}
	for _, unit := range units {
		if state := states[unit]; systemd.Exited(state) {
			c.log.Info("unit exited", "unit", unit, "state", state)
			c.release(ctx, unit, state == "failed")
		}
	}
}

// release cleans up after a unit that has exited or never started, then
// frees its slot and memory. Unless scale-down already deregistered the
// runner, its registration is queued for removal.
func (c *Controller) release(ctx context.Context, unit string, failed bool) {
	ctx = context.WithoutCancel(ctx)
	c.mu.Lock()
	r := c.runners[unit]
	c.mu.Unlock()
	if r == nil {
		return
	}

	// The slot stays taken until its file is gone, so a new runner's file
	// can't be the one removed.
	if err := removeJITFile(c.pools[r.Pool].JITConfigDir, r.Slot); err != nil {
		c.log.Error("couldn't remove the JIT config file", "unit", unit, "error", err)
	}
	if failed {
		if err := c.sd.ResetFailed(ctx, unit); err != nil {
			c.log.Warn("couldn't reset the failed unit", "unit", unit, "error", err)
		}
	}

	c.mu.Lock()
	delete(c.runners, unit)
	if !r.foreign && !r.deregistered {
		now := c.now()
		c.removals = append(c.removals, &removal{ScaleSet: r.ScaleSet, RunnerID: r.ID, Name: r.Name, Since: now, Next: now})
		// Exiting without running a job is a failure whatever the unit's
		// result; otherwise a runner that crashes on start is replaced in a
		// tight loop for as long as its job waits.
		if s := c.byName[r.ScaleSet]; s != nil && r.started && !r.busy {
			retryAt := c.backOff(s)
			c.log.Warn("the runner exited without running a job", "unit", unit, "scale_set", r.ScaleSet, "retry_at", retryAt)
		}
	}
	c.mu.Unlock()
	c.save()
	c.signal()
}

// backOff records a failure for s and delays its next start. Call with c.mu
// held.
func (c *Controller) backOff(s *scaleSet) time.Time {
	s.failures++
	s.retryAt = c.now().Add(backoff(startRetry, startRetryUp, s.failures))
	return s.retryAt
}

const (
	removalRetry   = time.Minute
	removalRetryUp = 15 * time.Minute
	// removalGiveUp: GitHub itself removes an ephemeral runner offline for a
	// day.
	removalGiveUp = 24 * time.Hour
)

// removeRegistrations retries the queued registration removals that are due.
// An ephemeral runner deregisters itself after its job, so most find nothing
// to remove.
func (c *Controller) removeRegistrations(ctx context.Context) {
	c.mu.Lock()
	now := c.now()
	var due []*removal
	for _, rm := range c.removals {
		if !now.Before(rm.Next) {
			due = append(due, rm)
		}
	}
	c.mu.Unlock()

	for _, rm := range due {
		var err error
		s := c.byName[rm.ScaleSet]
		if s != nil {
			err = s.client.RemoveRunner(ctx, rm.RunnerID, rm.Name)
		}
		c.mu.Lock()
		switch {
		case s == nil:
			c.log.Warn("can't remove the registration of a runner whose scale set is no longer configured", "runner", rm.Name, "scale_set", rm.ScaleSet)
			c.dropRemoval(rm)
		case err == nil:
			c.log.Debug("registration removed", "runner", rm.Name)
			c.dropRemoval(rm)
		case now.Sub(rm.Since) > removalGiveUp:
			c.log.Warn("giving up removing a runner's registration", "runner", rm.Name, "error", err)
			c.dropRemoval(rm)
		default:
			rm.Attempts++
			rm.Next = now.Add(backoff(removalRetry, removalRetryUp, rm.Attempts))
			c.log.Warn("couldn't remove a runner's registration; will retry", "runner", rm.Name, "retry_at", rm.Next, "error", err)
		}
		c.mu.Unlock()
	}
	if len(due) > 0 {
		c.save()
	}
}

func (c *Controller) dropRemoval(rm *removal) {
	c.removals = slices.DeleteFunc(c.removals, func(x *removal) bool { return x == rm })
}

func (c *Controller) schedule(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	starts := c.plan()
	c.mu.Unlock()
	if len(starts) > 0 {
		c.save()
	}
	for _, r := range starts {
		c.starts.Go(func() { c.start(ctx, r) })
	}
}

// plan reserves the runners to start now and returns them. Call with c.mu
// held.
//
// The budget counts each live unit at its pool's declared memory, never its
// usage. The scale set whose oldest uncovered job has waited longest goes
// first; if it doesn't fit, nothing else starts in its place, so pools of
// large runners can't be starved by small ones.
func (c *Controller) plan() []*runner {
	now := c.now()
	var used int64
	live := map[*scaleSet]int{}
	busy := map[*scaleSet]int{}
	inPool := map[string]int{}
	for _, r := range c.runners {
		used += int64(c.pools[r.Pool].Memory)
		inPool[r.Pool]++
		if s := c.byName[r.ScaleSet]; s != nil && !r.foreign && !r.deregistered && !r.done {
			live[s]++
			if r.busy {
				busy[s]++
			}
		}
	}
	need := func(s *scaleSet) int { return s.pool.MinRunners + s.assigned - live[s] }
	for _, s := range c.sets {
		switch {
		case need(s) <= 0:
			s.demandSince = time.Time{}
		case s.demandSince.IsZero():
			s.demandSince = now
		}
	}

	var out []*runner
	for {
		var best *scaleSet
		var bestKey priority
		for _, s := range c.sets {
			if need(s) <= 0 || now.Before(s.retryAt) {
				continue
			}
			if p := s.pool; p.MaxRunners > 0 && inPool[p.Name] >= p.MaxRunners {
				continue
			}
			key := priority{since: s.demandSince, index: s.index}
			// More jobs than runners means a job is waiting, rather than
			// only min_runners being short.
			if key.waiting = s.assigned > live[s]; key.waiting {
				// Runners not yet busy take the oldest waiting jobs; the
				// first job after those is the one still waiting.
				if t, ok := s.nthPending(live[s] - busy[s]); ok {
					key.since = t
				}
			}
			if best == nil || key.before(bestKey) {
				best, bestKey = s, key
			}
		}
		if best == nil {
			c.blocked = nil
			return out
		}
		mem := int64(best.pool.Memory)
		if used+mem > c.budget {
			if c.blocked != best {
				c.log.Info("the memory budget is full; this scale set is next", "scale_set", best.cfg.Name, "waiting_since", bestKey.since, "budget", config.ByteSize(c.budget), "used", config.ByteSize(used))
				c.blocked = best
			}
			return out
		}

		slot := c.freeSlot(best.pool.Name)
		r := &runner{
			record: record{
				Pool:       best.pool.Name,
				Slot:       slot,
				ScaleSet:   best.cfg.Name,
				ScaleSetID: best.id,
				Name:       c.runnerName(best.pool.Name, slot),
			},
			unit: unitName(best.pool, slot),
		}
		c.runners[r.unit] = r
		used += mem
		live[best]++
		inPool[best.pool.Name]++
		out = append(out, r)
	}
}

// priority orders the scale sets that need a runner.
type priority struct {
	// waiting: a job is waiting, not just a min_runners shortfall.
	waiting bool
	// since: the waiting job's queue time, or when the shortfall began.
	since time.Time
	index int
}

func (p priority) before(q priority) bool {
	if p.waiting != q.waiting {
		return p.waiting
	}
	if !p.since.Equal(q.since) {
		return p.since.Before(q.since)
	}
	return p.index < q.index
}

// nthPending returns the queue time of the n-th oldest pending job.
func (s *scaleSet) nthPending(n int) (time.Time, bool) {
	times := slices.SortedFunc(maps.Values(s.pending), func(a, b time.Time) int { return a.Compare(b) })
	n = max(n, 0)
	if n >= len(times) {
		return time.Time{}, false
	}
	return times[n], true
}

// freeSlot returns a pool's lowest slot with no unit in it.
func (c *Controller) freeSlot(pool string) int {
	taken := map[int]bool{}
	for _, r := range c.runners {
		if r.Pool == pool {
			taken[r.Slot] = true
		}
	}
	slot := 0
	for taken[slot] {
		slot++
	}
	return slot
}

func unitName(p *config.Pool, slot int) string {
	return p.Unit + "@" + strconv.Itoa(slot) + ".service"
}

// slotOf returns the slot of one of a pool's units, or false for a unit of
// its template whose instance isn't a slot number.
func slotOf(p *config.Pool, unit string) (int, bool) {
	inst, ok := strings.CutPrefix(unit, p.Unit+"@")
	if !ok {
		return 0, false
	}
	inst, ok = strings.CutSuffix(inst, ".service")
	if !ok {
		return 0, false
	}
	slot, err := strconv.Atoi(inst)
	if err != nil || slot < 0 || strconv.Itoa(slot) != inst {
		return 0, false
	}
	return slot, true
}

// runnerName is unique per start, so a slot can be reused while its previous
// runner's registration is still being removed.
func (c *Controller) runnerName(pool string, slot int) string {
	return fmt.Sprintf("%s-%s-%d-%s", c.owner, pool, slot, c.suffix())
}

func randomSuffix() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

const (
	startRetry   = 10 * time.Second
	startRetryUp = 5 * time.Minute
)

// start registers a reserved runner, writes its JIT config file and starts
// its unit. On failure the runner is released and its scale set backs off.
func (c *Controller) start(ctx context.Context, r *runner) {
	s := c.byName[r.ScaleSet]
	pool := c.pools[r.Pool]
	log := c.log.With("unit", r.unit, "scale_set", r.ScaleSet, "runner", r.Name)
	log.Info("starting a runner")

	jit, err := s.client.GenerateJITConfig(ctx, s.id, r.Name)
	if err != nil {
		c.abort(ctx, r, s, false, fmt.Errorf("registering the runner: %w", err))
		return
	}
	c.mu.Lock()
	r.ID = jit.RunnerID
	c.mu.Unlock()
	c.save()

	// Once registered, finish the start even if the controller is stopping,
	// rather than cleaning up under a unit systemd may be starting.
	ctx = context.WithoutCancel(ctx)
	if err := writeJITFile(pool.JITConfigDir, r.Slot, jit.Encoded); err != nil {
		c.abort(ctx, r, s, false, fmt.Errorf("writing the JIT config file: %w", err))
		return
	}
	if err := c.sd.Start(ctx, r.unit); err != nil {
		// systemctl can fail (a timeout, say) with the unit up anyway; keep
		// it rather than let it run untracked.
		states, serr := c.sd.ActiveStates(ctx, []string{r.unit})
		if state := states[r.unit]; serr != nil || state == "" || systemd.Exited(state) {
			c.abort(ctx, r, s, true, fmt.Errorf("starting the unit: %w", err))
			return
		}
		log.Warn("systemctl start failed, but the unit is running; keeping it", "state", states[r.unit], "error", err)
	}

	c.mu.Lock()
	r.started = true
	r.startedAt = c.now()
	c.mu.Unlock()
	log.Info("runner started", "runner_id", jit.RunnerID)
	c.signal()
}

// abort releases a runner whose start failed. unitFailed means systemd was
// asked to start it, so the unit may need resetting.
func (c *Controller) abort(ctx context.Context, r *runner, s *scaleSet, unitFailed bool, err error) {
	c.mu.Lock()
	retryAt := c.backOff(s)
	c.mu.Unlock()
	c.log.Error("couldn't start a runner", "unit", r.unit, "scale_set", r.ScaleSet, "retry_at", retryAt, "error", err)
	c.release(ctx, r.unit, unitFailed)
}

// backoff doubles from first, for each attempt after the first, up to most.
func backoff(first, most time.Duration, attempts int) time.Duration {
	d := first
	for i := 1; i < attempts && d < most; i++ {
		d *= 2
	}
	return min(d, most)
}

// save writes the state file. Call without c.mu: it takes the lock only to
// snapshot, so disk latency never holds up the sessions, and a stale
// snapshot never overwrites a newer one. Failures are only logged; recover
// works from systemd as well as the file.
func (c *Controller) save() {
	c.mu.Lock()
	c.seq++
	seq := c.seq
	st := &state{}
	for _, unit := range slices.Sorted(maps.Keys(c.runners)) {
		if r := c.runners[unit]; !r.foreign {
			st.Runners = append(st.Runners, r.record)
		}
	}
	for _, rm := range c.removals {
		copied := *rm
		st.Removals = append(st.Removals, &copied)
	}
	c.mu.Unlock()

	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	if seq <= c.written {
		return
	}
	if err := saveState(c.stateDir, st); err != nil {
		c.log.Error("couldn't save the runners' state", "error", err)
		return
	}
	c.written = seq
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// maxCapacity is the most runners s could have at once, reported to GitHub
// so it doesn't assign more jobs than that.
func (c *Controller) maxCapacity(s *scaleSet) int {
	if s.pool.MaxRunners > 0 {
		return s.pool.MaxRunners
	}
	return int(c.budget / int64(s.pool.Memory))
}
