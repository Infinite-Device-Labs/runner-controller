package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

// fakeClient is a scale set API that registers runners unless told to fail.
type fakeClient struct {
	mu        sync.Mutex
	id        int
	nextID    int64
	jitErr    error
	removeErr error
	// jitHook, when set, runs before each registration and may fail it.
	jitHook func(name string) error
	jits    []string // runner names registered
	removed []string // "id/name" of registrations removed
	session *fakeSession
	// openErrs fail the next OpenSession calls, one each.
	openErrs []error
	opens    int
}

func (f *fakeClient) EnsureScaleSet(ctx context.Context, spec actions.ScaleSetSpec) (int, error) {
	return f.id, nil
}

func (f *fakeClient) OpenSession(ctx context.Context, scaleSetID int, owner string) (actions.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if len(f.openErrs) > 0 {
		err := f.openErrs[0]
		f.openErrs = f.openErrs[1:]
		return nil, err
	}
	if f.session == nil {
		return nil, errors.New("no session")
	}
	return f.session, nil
}

func (f *fakeClient) GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (*actions.JITConfig, error) {
	if f.jitHook != nil {
		if err := f.jitHook(runnerName); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return nil, f.jitErr
	}
	f.nextID++
	f.jits = append(f.jits, runnerName)
	return &actions.JITConfig{RunnerID: f.nextID, Encoded: "jit-for-" + runnerName}, nil
}

func (f *fakeClient) RemoveRunner(ctx context.Context, runnerID int64, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, fmt.Sprintf("%d/%s", runnerID, name))
	return nil
}

func (f *fakeClient) removals() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

// fakeSession plays back a scripted list of messages, then fails.
type fakeSession struct {
	mu       sync.Mutex
	stats    actions.Statistics
	messages []*actions.Message
	deleted  []int
	acquired []int64
	closed   bool
	lastIDs  []int
	// onGet runs at each GetMessage, before the script is read.
	onGet      func()
	acquireErr error
}

func (f *fakeSession) Statistics() actions.Statistics { return f.stats }

func (f *fakeSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*actions.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastIDs = append(f.lastIDs, lastMessageID)
	if f.onGet != nil {
		f.onGet()
	}
	if len(f.messages) == 0 {
		return nil, errors.New("script over")
	}
	m := f.messages[0]
	f.messages = f.messages[1:]
	return m, nil
}

func (f *fakeSession) DeleteMessage(ctx context.Context, messageID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, messageID)
	return nil
}

func (f *fakeSession) AcquireJobs(ctx context.Context, requestIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acquireErr != nil {
		return f.acquireErr
	}
	f.acquired = append(f.acquired, requestIDs...)
	return nil
}

func (f *fakeSession) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeSystemd keeps each unit's ActiveState. A started unit is active until
// the test or Stop changes it.
type fakeSystemd struct {
	mu       sync.Mutex
	states   map[string]string
	startErr error
	// upDespiteErr leaves the unit active when Start returns startErr.
	upDespiteErr bool
	// startHook, when set, runs first and may fail the start.
	startHook func(unit string) error
	started   []string
	reset     []string
	stopErr   error
	stopped   []string
}

func newFakeSystemd() *fakeSystemd { return &fakeSystemd{states: map[string]string{}} }

func (f *fakeSystemd) Start(ctx context.Context, unit string) error {
	if f.startHook != nil {
		if err := f.startHook(unit); err != nil {
			f.set(unit, "failed")
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		f.states[unit] = "failed"
		if f.upDespiteErr {
			f.states[unit] = "active"
		}
		return f.startErr
	}
	f.states[unit] = "active"
	f.started = append(f.started, unit)
	return nil
}

func (f *fakeSystemd) ActiveStates(ctx context.Context, units []string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]string{}
	for _, u := range units {
		out[u] = f.state(u)
	}
	return out, nil
}

func (f *fakeSystemd) state(unit string) string {
	if s, ok := f.states[unit]; ok {
		return s
	}
	return "inactive"
}

func (f *fakeSystemd) List(ctx context.Context, pattern string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := strings.TrimSuffix(pattern, "*.service")
	out := map[string]string{}
	for u, s := range f.states {
		if strings.HasPrefix(u, prefix) {
			out[u] = s
		}
	}
	return out, nil
}

func (f *fakeSystemd) Stop(ctx context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, unit)
	f.states[unit] = "inactive"
	return nil
}

func (f *fakeSystemd) ResetFailed(ctx context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reset = append(f.reset, unit)
	if f.states[unit] == "failed" {
		delete(f.states, unit)
	}
	return nil
}

// set sets a unit's state, as if it had started or exited.
func (f *fakeSystemd) set(unit, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[unit] = state
}

// takeStarted returns the units started since it was last called, sorted:
// starts run concurrently, so their order within one step isn't fixed.
func (f *fakeSystemd) takeStarted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.started
	f.started = nil
	slices.Sort(out)
	return out
}

// harness is a controller with fakes on every side and a clock the test
// moves.
type harness struct {
	t       *testing.T
	c       *Controller
	sd      *fakeSystemd
	clients map[string]*fakeClient
	now     time.Time
	jobID   int64

	sleptMu sync.Mutex
	slept   []time.Duration
}

func (h *harness) sleeps() []time.Duration {
	h.sleptMu.Lock()
	defer h.sleptMu.Unlock()
	return append([]time.Duration(nil), h.slept...)
}

// pool and set build a configuration tersely.
func pool(unit, memory string, minRunners, maxRunners int) *config.Pool {
	m, err := config.ParseByteSize(memory)
	if err != nil {
		panic(err)
	}
	return &config.Pool{Unit: unit, Memory: m, MinRunners: minRunners, MaxRunners: maxRunners}
}

func set(name, poolName string) *config.ScaleSet {
	return &config.ScaleSet{
		Name: name,
		URL:  "https://github.com/example-org",
		Pool: poolName,
		GitHubApp: config.GitHubApp{
			ID: "1", InstallationID: 2, PrivateKeyPath: "/dev/null",
		},
	}
}

func newHarness(t *testing.T, budget string, pools map[string]*config.Pool, sets ...*config.ScaleSet) *harness {
	t.Helper()
	dir := t.TempDir()
	b, err := config.ParseByteSize(budget)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range pools {
		p.JITConfigDir = filepath.Join(dir, "jit", name)
	}
	cfg := &config.Config{MemoryBudget: b, StateDir: filepath.Join(dir, "state"), Pools: pools, ScaleSets: sets}
	if err := cfg.Check(); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, sd: newFakeSystemd(), clients: map[string]*fakeClient{}, now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	clients := map[string]actions.Client{}
	for i, s := range sets {
		fc := &fakeClient{id: 100 + i}
		h.clients[s.Name] = fc
		clients[s.Name] = fc
	}
	c, err := New(cfg, clients, h.sd, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c.owner = "host"
	n := 0
	c.suffix = func() string { n++; return fmt.Sprintf("r%d", n) }
	c.now = func() time.Time { return h.now }
	c.emptyPollWait = 0
	c.sleep = func(ctx context.Context, d time.Duration) {
		h.sleptMu.Lock()
		defer h.sleptMu.Unlock()
		h.slept = append(h.slept, d)
	}
	for _, s := range c.sets {
		s.id = h.clients[s.cfg.Name].id
	}
	h.c = c
	return h
}

// step runs one reconcile, waits for the starts it began, and returns the
// units they started.
func (h *harness) step() []string {
	h.t.Helper()
	h.c.reconcile(context.Background())
	h.c.starts.Wait()
	return h.sd.takeStarted()
}

// demand reports a scale set's statistics, and newly assigned jobs queued at
// the given times, as a message would.
func (h *harness) demand(name string, assigned, running int, queued ...time.Time) {
	s := h.c.byName[name]
	msg := &actions.Message{}
	for _, t := range queued {
		h.jobID++
		msg.Assigned = append(msg.Assigned, actions.Job{RequestID: h.jobID, QueueTime: t})
	}
	h.c.observe(s, actions.Statistics{AssignedJobs: assigned, RunningJobs: running}, msg)
}

// jobStarts reports that a scale set's oldest waiting job has started on its
// idle runner in the lowest slot, with the statistics that follow.
func (h *harness) jobStarts(name string, assigned, running int) {
	h.t.Helper()
	s := h.c.byName[name]
	var job int64
	for id, t := range s.pending {
		if job == 0 || t.Before(s.pending[job]) {
			job = id
		}
	}
	var idle *runner
	for _, r := range h.c.runners {
		if r.ScaleSet == name && !r.busy && (idle == nil || r.Slot < idle.Slot) {
			idle = r
		}
	}
	if job == 0 || idle == nil {
		h.t.Fatalf("scale set %q has no waiting job or no idle runner", name)
	}
	h.c.observe(s, actions.Statistics{AssignedJobs: assigned, RunningJobs: running}, &actions.Message{
		Started: []actions.Job{{RequestID: job, RunnerName: idle.Name}},
	})
}

// at returns the harness's start time plus d.
func (h *harness) at(d time.Duration) time.Time {
	return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Add(d)
}
