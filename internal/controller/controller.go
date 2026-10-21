// Package controller decides when to start a runner, and for which scale set,
// within one memory budget. systemd runs the runners; the controller starts
// their units, writes each one's JIT config file, and cleans up after them.
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
	// tick is how often units are checked when nothing else wakes the
	// controller.
	tick time.Duration
	// sessionRetry is the first wait before a failed session is reopened.
	sessionRetry time.Duration
	// emptyPollWait is the least time between the starts of two polls that
	// bring no message, in case GitHub stops holding them open.
	emptyPollWait time.Duration
	// sleep waits for d or until ctx ends; replaced in tests.
	sleep func(ctx context.Context, d time.Duration)

	mu sync.Mutex
	// runners are the units started and not yet seen to exit, by unit name.
	// Their pools' memory is what the budget counts.
	runners  map[string]*runner
	removals []*removal
	// blocked is the scale set the budget last held back, so it is logged
	// once rather than on every tick.
	blocked *scaleSet
	// seq numbers the snapshots of the state taken to be saved.
	seq uint64

	// saveMu serialises writes of the state file. written is the newest
	// snapshot on disk, so an older one that lost the race isn't written
	// over it.
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

	// assigned is GitHub's count of jobs assigned to the scale set, waiting
	// or running: the number of runners it needs.
	assigned int
	// pending are the jobs assigned and not yet started, with when each was
	// queued. Messages carry at most 50 events, so this can miss jobs; it
	// only orders the scale sets, and assigned decides how many runners.
	pending map[int64]time.Time
	// demandSince is when the scale set last went from needing no runner to
	// needing one.
	demandSince time.Time
	// failures counts the runners in a row that failed to start or exited
	// without running a job, and retryAt holds the next start back.
	failures int
	retryAt  time.Time
}

// runner is a unit the controller started, or found running at startup.
type runner struct {
	record
	unit string
	// started is set once systemctl start has returned. Before then the
	// unit isn't checked for having exited, though its memory counts.
	started bool
	// startedAt is when systemctl start returned, or when a running unit
	// was adopted.
	startedAt time.Time
	// busy is set once it is seen to run a job, and done once that job
	// has completed: the runner is about to exit, and no longer counts
	// towards its scale set's runners.
	busy bool
	done bool
	// adopted is a unit found running at startup whose job isn't known. It
	// counts as busy until GitHub says otherwise.
	adopted bool
	// foreign is a running unit in a pool's template that the controller
	// has no record of. It counts against the budget until it exits.
	foreign bool

	// Scaling down: stopping is set while its registration is being
	// removed, deregistered once that succeeded, and stopRequested once
	// systemd took the stop. A deregistered runner can't take a job, so it
	// no longer counts towards its scale set's runners, though its memory
	// counts until its unit exits. nextScaleDown holds back another attempt
	// after one failed.
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

// Run creates or reuses the scale sets, cleans up after an earlier run, and
// then starts runners as jobs arrive until ctx ends. Units still running
// then are left to finish their jobs; the next run adopts them.
func (c *Controller) Run(ctx context.Context) error {
	for _, s := range c.sets {
		id, err := s.client.EnsureScaleSet(ctx, actions.ScaleSetSpec{
			Name:        s.cfg.Name,
			RunnerGroup: s.cfg.RunnerGroup,
			Labels:      s.cfg.Labels,
		})
		if err != nil {
			return err
		}
		s.id = id
		c.log.Info("scale set ready", "scale_set", s.cfg.Name, "id", id, "pool", s.pool.Name)
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

// reconcile frees what exited units held, retries the registrations still
// to remove, and starts what the budget allows.
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
// frees its slot and its memory. Its registration is queued for removal.
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
	// A runner stopped for being idle was deregistered first, and didn't
	// fail.
	if !r.foreign && !r.deregistered {
		now := c.now()
		c.removals = append(c.removals, &removal{ScaleSet: r.ScaleSet, RunnerID: r.ID, Name: r.Name, Since: now, Next: now})
		// A runner that exits without having run a job has failed, however
		// its unit ended. Otherwise one that crashes as it starts would be
		// replaced at once, for as long as its job waits.
		if s := c.byName[r.ScaleSet]; s != nil && r.started && !r.busy {
			retryAt := c.backOff(s)
			c.log.Warn("the runner exited without running a job", "unit", unit, "scale_set", r.ScaleSet, "retry_at", retryAt)
		}
	}
	c.mu.Unlock()
	c.save()
	c.signal()
}

// backOff counts a failure against a scale set and holds its next start
// back. It is called with c.mu held.
func (c *Controller) backOff(s *scaleSet) time.Time {
	s.failures++
	s.retryAt = c.now().Add(backoff(startRetry, startRetryUp, s.failures))
	return s.retryAt
}

const (
	removalRetry   = time.Minute
	removalRetryUp = 15 * time.Minute
	// removalGiveUp is how long a registration is retried. GitHub removes
	// an ephemeral runner that has been offline for a day by itself.
	removalGiveUp = 24 * time.Hour
)

// removeRegistrations removes the registrations of runners whose units have
// gone. An ephemeral runner removes its own after its job, so most of these
// find nothing to do.
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

// plan reserves the runners to start now, in order, and returns them. It is
// called with c.mu held.
//
// The budget is counted from the memory each pool declares, for every unit
// started and not yet exited, never from what the units use. While it is
// full, the next free memory goes to the scale set whose oldest waiting job
// has waited longest. That scale set is held back rather than passed over
// when it doesn't fit, even if another one would, so a pool with large
// runners can't be starved by pools with small ones.
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
	// waiting is a job waiting, rather than only min_runners being short.
	waiting bool
	// since is when the waiting job was queued, or when the shortfall began.
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

// runnerName is unique per start, so a slot can be reused while its last
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
// its unit. If any step fails, the runner is released and its scale set
// waits before it is tried again.
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

	// Once registered, the start is finished even if the controller is
	// stopping, so a unit systemd is already starting isn't cleaned up
	// under it. systemd's own start timeout bounds the wait.
	ctx = context.WithoutCancel(ctx)
	if err := writeJITFile(pool.JITConfigDir, r.Slot, jit.Encoded); err != nil {
		c.abort(ctx, r, s, false, fmt.Errorf("writing the JIT config file: %w", err))
		return
	}
	if err := c.sd.Start(ctx, r.unit); err != nil {
		// systemctl can fail with the unit up, for one when it times out
		// waiting. A running unit is kept, or it would run untracked.
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

// abort releases a runner whose start failed. unitFailed is set when
// systemd was asked to start the unit, so it may need resetting.
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

// save writes the record of runners and removals. It is called without
// c.mu, so the disk's latency never holds up the sessions. A failure is
// logged rather than returned: the controller carries on, and a restart
// cleans up from the units systemd still knows about.
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

// maxCapacity is the most runners a scale set could have at once, which
// GitHub uses to avoid assigning it more jobs than it can take.
func (c *Controller) maxCapacity(s *scaleSet) int {
	if s.pool.MaxRunners > 0 {
		return s.pool.MaxRunners
	}
	return int(c.budget / int64(s.pool.Memory))
}
