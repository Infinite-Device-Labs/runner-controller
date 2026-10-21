package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

func equal[T comparable](t *testing.T, what string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %v\nwant %v", what, got, want)
	}
}

func TestBudgetCountsDeclaredLimits(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))

	h.demand("a", 5, 0, h.at(0), h.at(1), h.at(2), h.at(3), h.at(4))
	// 4G each in a 10G budget: two fit, the third would declare 12G.
	equal(t, "started", h.step(), []string{"ci-runner@0.service", "ci-runner@1.service"})

	// Nothing changes while they run, however often it is asked.
	equal(t, "started", h.step(), nil)
	equal(t, "started", h.step(), nil)

	// One runs its job and exits: its memory and its slot go to the next
	// runner.
	h.jobStarts("a", 5, 1)
	h.sd.set("ci-runner@0.service", "inactive")
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})
}

func TestJITConfigFile(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()

	path := filepath.Join(h.c.pools["p"].JITConfigDir, "0.env")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("mode %o, want 600", mode)
	}
	b, _ := os.ReadFile(path)
	if got, want := string(b), "ACTIONS_RUNNER_INPUT_JITCONFIG=jit-for-host-p-0-r1\n"; got != want {
		t.Errorf("content %q, want %q", got, want)
	}

	// The unit exits: the file goes with it, and the registration is
	// removed.
	h.sd.set("ci-runner@0.service", "inactive")
	h.demand("a", 0, 0)
	h.step()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the JIT file is still there: %v", err)
	}
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})
}

func TestMaxRunnersCapsAPool(t *testing.T) {
	h := newHarness(t, "100G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 2)}, set("a", "p"))
	h.demand("a", 5, 0)
	equal(t, "started", h.step(), []string{"ci-runner@0.service", "ci-runner@1.service"})
	if got := h.c.maxCapacity(h.c.byName["a"]); got != 2 {
		t.Errorf("max capacity %d, want 2", got)
	}
}

func TestMaxCapacityWithoutACapIsWhatTheBudgetHolds(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "3G", 0, 0)}, set("a", "p"))
	if got := h.c.maxCapacity(h.c.byName["a"]); got != 3 {
		t.Errorf("max capacity %d, want 3", got)
	}
}

func TestMinRunnersAreKeptStarted(t *testing.T) {
	h := newHarness(t, "100G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 2, 0)}, set("a", "p"))
	equal(t, "started", h.step(), []string{"ci-runner@0.service", "ci-runner@1.service"})

	// A job takes one: another is started, so two stay idle.
	h.demand("a", 1, 0, h.at(0))
	h.jobStarts("a", 1, 1)
	equal(t, "started", h.step(), []string{"ci-runner@2.service"})
}

func TestTheOldestWaitingJobGetsTheNextMemory(t *testing.T) {
	h := newHarness(t, "4G",
		map[string]*config.Pool{"pa": pool("runner-a", "2G", 0, 0), "pb": pool("runner-b", "2G", 0, 0)},
		set("a", "pa"), set("b", "pb"))

	// a's jobs were queued at 0 and 3 minutes, b's at 1 and 2.
	h.demand("a", 2, 0, h.at(0), h.at(3*time.Minute))
	h.demand("b", 2, 0, h.at(1*time.Minute), h.at(2*time.Minute))
	// Room for two: a's oldest job, then b's.
	equal(t, "started", h.step(), []string{"runner-a@0.service", "runner-b@0.service"})
	h.jobStarts("a", 2, 1)
	h.jobStarts("b", 2, 1)

	// a's runner finishes its job and exits. b's second job (2m) has waited
	// longer than a's (3m), so b gets the memory.
	h.sd.set("runner-a@0.service", "inactive")
	h.demand("a", 1, 0)
	equal(t, "started", h.step(), []string{"runner-b@1.service"})

	// b's first runner exits too: now a's job is the only one waiting.
	h.sd.set("runner-b@0.service", "inactive")
	h.demand("b", 1, 0)
	equal(t, "started", h.step(), []string{"runner-a@0.service"})
}

func TestIdleRunnersCoverTheOldestJobs(t *testing.T) {
	h := newHarness(t, "4G",
		map[string]*config.Pool{"pa": pool("runner-a", "2G", 0, 0), "pb": pool("runner-b", "2G", 0, 0)},
		set("a", "pa"), set("b", "pb"))
	// a has jobs queued at 0 and 5m, b one at 1m. a's first runner will
	// take its job at 0, which leaves its 5m job behind b's at 1m.
	h.demand("a", 2, 0, h.at(0), h.at(5*time.Minute))
	h.demand("b", 1, 0, h.at(time.Minute))
	equal(t, "started", h.step(), []string{"runner-a@0.service", "runner-b@0.service"})
}

func TestWaitingJobsComeBeforeMinRunners(t *testing.T) {
	h := newHarness(t, "2G",
		map[string]*config.Pool{"warm": pool("runner-warm", "2G", 1, 0), "cold": pool("runner-cold", "2G", 0, 0)},
		set("warm", "warm"), set("cold", "cold"))
	// warm has wanted a runner since 0, with no room for it.
	h.c.budget = 0
	equal(t, "started", h.step(), nil)

	// cold's job, queued an hour later, still goes first.
	h.now = h.at(time.Hour)
	h.demand("cold", 1, 0, h.at(time.Hour))
	h.c.budget = 2 << 30
	equal(t, "started", h.step(), []string{"runner-cold@0.service"})
}

func TestTheNextScaleSetIsHeldBackNotPassedOver(t *testing.T) {
	h := newHarness(t, "8G",
		map[string]*config.Pool{"big": pool("runner-big", "6G", 0, 0), "small": pool("runner-small", "2G", 0, 0)},
		set("big", "big"), set("small", "small"))
	h.demand("small", 2, 0, h.at(0), h.at(time.Second))
	equal(t, "started", h.step(), []string{"runner-small@0.service", "runner-small@1.service"})
	h.jobStarts("small", 2, 1)
	h.jobStarts("small", 2, 2)

	// big's job is older than small's third. 4G is free: small's would fit,
	// big's wouldn't. Nothing starts, so big isn't starved.
	h.now = h.at(3 * time.Minute)
	h.demand("big", 1, 0, h.at(time.Minute))
	h.demand("small", 3, 2, h.at(2*time.Minute))
	equal(t, "started", h.step(), nil)

	// One small runner exits: 6G is free, and it goes to big.
	h.sd.set("runner-small@0.service", "inactive")
	h.demand("small", 2, 1)
	equal(t, "started", h.step(), []string{"runner-big@0.service"})
}

func TestAFailedStartIsCleanedUpAndRetriedLater(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.sd.startErr = errors.New("no such unit")
	h.demand("a", 1, 0, h.at(0))
	h.step()

	if len(h.c.runners) != 0 {
		t.Errorf("the failed runner still holds memory: %v", h.c.runners)
	}
	if _, err := os.Stat(filepath.Join(h.c.pools["p"].JITConfigDir, "0.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the JIT file was left behind: %v", err)
	}
	if !slices.Contains(h.sd.reset, "ci-runner@0.service") {
		t.Error("the failed unit wasn't reset")
	}
	h.step()
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})

	// Not retried until the backoff has passed.
	h.sd.startErr = nil
	equal(t, "started", h.step(), nil)
	h.now = h.at(startRetry)
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})
}

func TestARegistrationIsRetriedWhileItsJobStillRuns(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()

	// The unit dies mid-job. GitHub still counts the job as running.
	h.clients["a"].removeErr = actions.ErrJobStillRunning
	h.jobStarts("a", 1, 1)
	h.sd.set("ci-runner@0.service", "failed")
	h.step()
	if !slices.Contains(h.sd.reset, "ci-runner@0.service") {
		t.Error("the failed unit wasn't reset")
	}
	if len(h.c.removals) != 1 || h.c.removals[0].Attempts != 1 {
		t.Fatalf("removals %+v, want one attempted once", h.c.removals)
	}

	h.clients["a"].removeErr = nil
	h.step()
	equal(t, "removed too early", h.clients["a"].removals(), nil)
	h.now = h.at(removalRetry)
	h.step()
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})
	if len(h.c.removals) != 0 {
		t.Errorf("removals left: %+v", h.c.removals)
	}
}

func TestStartedRunnersAreRecorded(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.demand("a", 2, 0, h.at(0), h.at(1))
	h.step()

	st, err := loadState(h.c.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var ids []int64
	for _, r := range st.Runners {
		names = append(names, r.Name)
		ids = append(ids, r.ID)
		if r.ScaleSet != "a" || r.ScaleSetID != 100 || r.Pool != "p" {
			t.Errorf("record %+v", r)
		}
	}
	// The two registrations ran concurrently, so either may have ID 1.
	slices.Sort(ids)
	equal(t, "names", names, []string{"host-p-0-r1", "host-p-1-r2"})
	equal(t, "ids", ids, []int64{1, 2})
}

func TestBackoff(t *testing.T) {
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{{1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {10, 5 * time.Second}} {
		if got := backoff(time.Second, 5*time.Second, tc.attempts); got != tc.want {
			t.Errorf("backoff after %d: %v, want %v", tc.attempts, got, tc.want)
		}
	}
}

func TestSlotOf(t *testing.T) {
	p := &config.Pool{Unit: "ci-runner"}
	for unit, want := range map[string]int{"ci-runner@0.service": 0, "ci-runner@12.service": 12} {
		if got, ok := slotOf(p, unit); !ok || got != want {
			t.Errorf("slotOf(%q) = %d, %v", unit, got, ok)
		}
	}
	for _, unit := range []string{"ci-runner@x.service", "ci-runner@01.service", "ci-runner@-1.service", "ci-runner-2@1.service", "ci-runner@1.socket"} {
		if _, ok := slotOf(p, unit); ok {
			t.Errorf("slotOf(%q) took it for a slot", unit)
		}
	}
}

func TestNothingStartsOnceStopping(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.c.reconcile(ctx)
	h.c.starts.Wait()
	equal(t, "started", h.sd.takeStarted(), nil)
}

func TestARunnerThatDiesWithoutAJobHoldsItsScaleSetBack(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})

	// The unit starts, then dies before its runner takes the job, which
	// stays assigned. It isn't replaced at once.
	h.sd.set("ci-runner@0.service", "failed")
	equal(t, "started", h.step(), nil)
	equal(t, "started", h.step(), nil)
	if got := len(h.clients["a"].jits); got != 1 {
		t.Errorf("%d registrations, want 1", got)
	}
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})

	// After the backoff it is tried again, and a second death waits twice
	// as long.
	h.now = h.at(startRetry)
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})
	h.sd.set("ci-runner@0.service", "inactive")
	h.step()
	h.now = h.at(2 * startRetry)
	equal(t, "started too soon", h.step(), nil)
	h.now = h.at(3 * startRetry)
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})

	// A runner that runs its job clears the failures: the next one that
	// dies waits the first backoff again.
	h.jobStarts("a", 1, 1)
	if s := h.c.byName["a"]; s.failures != 0 {
		t.Errorf("failures %d after a job started, want 0", s.failures)
	}
}

func TestAStartThatErrsWithTheUnitUpKeepsTheRunner(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "4G", 0, 0)}, set("a", "p"))
	h.sd.startErr = errors.New("timed out waiting for the start job")
	h.sd.upDespiteErr = true
	h.demand("a", 1, 0, h.at(0))
	h.step()

	r := h.c.runners["ci-runner@0.service"]
	if r == nil || !r.started {
		t.Fatalf("the running unit was dropped: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(h.c.pools["p"].JITConfigDir, "0.env")); err != nil {
		t.Errorf("its JIT file was removed: %v", err)
	}
	if n := len(h.c.removals); n != 0 {
		t.Errorf("%d removals queued for a running runner", n)
	}
}

func TestASharedPoolsCapCoversEveryScaleSetAndForeignUnits(t *testing.T) {
	h := newHarness(t, "100G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 3)}, set("a", "p"), set("b", "p"))
	// A unit with no record is running in the pool.
	h.sd.set("ci-runner@0.service", "active")
	if err := h.c.recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Two of the cap's three remain. b's job is the older, so b gets the
	// first, then a.
	h.demand("a", 2, 0, h.at(2*time.Minute), h.at(3*time.Minute))
	h.demand("b", 2, 0, h.at(time.Minute), h.at(4*time.Minute))
	equal(t, "started", h.step(), []string{"ci-runner@1.service", "ci-runner@2.service"})
	sets := map[string]string{}
	for unit, r := range h.c.runners {
		sets[unit] = r.ScaleSet
	}
	if sets["ci-runner@1.service"] == sets["ci-runner@2.service"] {
		t.Errorf("both runners went to %q, want one each", sets["ci-runner@1.service"])
	}
	if got := h.c.maxCapacity(h.c.byName["a"]); got != 3 {
		t.Errorf("max capacity %d, want the pool's cap", got)
	}
}

func TestMinRunnersShortestLongestComeFirst(t *testing.T) {
	h := newHarness(t, "2G",
		map[string]*config.Pool{"x": pool("runner-x", "2G", 1, 0), "y": pool("runner-y", "2G", 1, 0)},
		set("x", "x"), set("y", "y"))
	// Both have been short since 0; x is first in the file.
	equal(t, "started", h.step(), []string{"runner-x@0.service"})
	x := h.c.byName["x"]

	// x's runner runs a job: x is satisfied, y still waits for memory.
	h.c.observe(x, actions.Statistics{}, &actions.Message{Completed: []actions.Job{{RequestID: 1, RunnerName: "host-x-0-r1"}}})
	equal(t, "started", h.step(), nil)
	if !x.demandSince.IsZero() {
		t.Error("x's shortfall wasn't cleared once met")
	}

	// An hour later x's runner exits. x has been short since now, y since
	// 0, so y gets the memory.
	h.now = h.at(time.Hour)
	h.sd.set("runner-x@0.service", "inactive")
	equal(t, "started", h.step(), []string{"runner-y@0.service"})
}
