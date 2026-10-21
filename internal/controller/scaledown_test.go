package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

func TestASurplusIdleRunnerIsStoppedAndItsMemoryReused(t *testing.T) {
	h := newHarness(t, "4G",
		map[string]*config.Pool{"pa": pool("runner-a", "2G", 0, 0), "pb": pool("runner-b", "2G", 0, 0)},
		set("a", "pa"), set("b", "pb"))
	h.demand("a", 2, 0, h.at(0), h.at(1))
	equal(t, "started", h.step(), []string{"runner-a@0.service", "runner-a@1.service"})

	// a's jobs are cancelled before a runner takes them, and b's job waits
	// for memory a's idle runners hold.
	h.demand("a", 0, 0)
	h.demand("b", 1, 0, h.at(time.Minute))
	equal(t, "started within the grace period", h.step(), nil)
	equal(t, "stopped within the grace period", h.sd.stopped, nil)

	// After it, both are deregistered, then stopped.
	h.now = h.at(idleGrace)
	h.step()
	equal(t, "removed", removedNames(h.clients["a"]), []string{"host-pa-0-r1", "host-pa-1-r2"})
	equal(t, "stopped", h.sd.stopped, []string{"runner-a@0.service", "runner-a@1.service"})

	// Once their units have exited, b gets the memory. Neither counts as a
	// failure, and their registrations aren't removed twice.
	equal(t, "started", h.step(), []string{"runner-b@0.service"})
	if s := h.c.byName["a"]; s.failures != 0 {
		t.Errorf("a has %d failures, want 0", s.failures)
	}
	h.step()
	if got := len(h.clients["a"].removals()); got != 2 {
		t.Errorf("%d removals, want 2", got)
	}
}

func TestOnlyTheSurplusIsStopped(t *testing.T) {
	h := newHarness(t, "100G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 1, 0)}, set("a", "p"))
	// min_runners 1 and three jobs: four runners.
	h.demand("a", 3, 0, h.at(0), h.at(1), h.at(2))
	equal(t, "started", h.step(), []string{"ci-runner@0.service", "ci-runner@1.service", "ci-runner@2.service", "ci-runner@3.service"})
	h.jobStarts("a", 3, 1)

	// Two jobs are cancelled: one job and min_runners need two runners.
	// The busy one stays, and two idle ones are stopped.
	h.now = h.at(idleGrace)
	h.demand("a", 1, 1)
	h.step()
	if len(h.sd.stopped) != 2 {
		t.Fatalf("stopped %v, want two", h.sd.stopped)
	}
	for _, unit := range h.sd.stopped {
		if h.c.runners[unit] != nil && h.c.runners[unit].busy {
			t.Errorf("stopped the busy runner %s", unit)
		}
	}
}

func TestARunnerWhoseJobCompletedIsntCountedOrStopped(t *testing.T) {
	h := newHarness(t, "100G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 1, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	equal(t, "started", h.step(), []string{"ci-runner@0.service", "ci-runner@1.service"})
	h.jobStarts("a", 1, 1)

	// The job completes; its runner is about to exit. The idle one is the
	// pool's min_runners, and stays.
	h.now = h.at(idleGrace)
	busy := ""
	for _, r := range h.c.runners {
		if r.busy {
			busy = r.Name
		}
	}
	h.c.observe(h.c.byName["a"], actions.Statistics{}, &actions.Message{Completed: []actions.Job{{RequestID: 1, RunnerName: busy}}})
	equal(t, "started", h.step(), nil)
	equal(t, "stopped", h.sd.stopped, nil)
}

func TestARunnerThatTookAJobIsLeftToRunIt(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()

	// The statistics say no job, but GitHub refuses the removal: the
	// runner took one as it was chosen.
	h.now = h.at(idleGrace)
	h.demand("a", 0, 0)
	h.clients["a"].removeErr = actions.ErrJobStillRunning
	h.step()
	equal(t, "stopped", h.sd.stopped, nil)
	if r := h.c.runners["ci-runner@0.service"]; r == nil || !r.busy {
		t.Errorf("the runner isn't marked busy: %+v", r)
	}

	// Being busy, it isn't tried again.
	h.clients["a"].removeErr = errors.New("unexpected call")
	h.now = h.at(time.Hour)
	h.step()
	equal(t, "stopped", h.sd.stopped, nil)
}

func TestAFailedRemovalOrStopIsRetriedLater(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()
	h.demand("a", 0, 0)
	h.now = h.at(idleGrace)

	h.clients["a"].removeErr = errors.New("502 Bad Gateway")
	h.step()
	h.clients["a"].removeErr = nil
	h.step()
	equal(t, "removed before the retry", h.clients["a"].removals(), nil)

	h.now = h.at(idleGrace + scaleDownRetry)
	h.sd.stopErr = errors.New("dbus timeout")
	h.step()
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})
	equal(t, "stopped", h.sd.stopped, nil)

	// Deregistered, it no longer counts: a new job starts another runner.
	h.demand("a", 1, 0, h.at(time.Hour))
	equal(t, "started", h.step(), []string{"ci-runner@1.service"})

	h.sd.stopErr = nil
	h.now = h.at(idleGrace + 2*scaleDownRetry)
	h.step()
	equal(t, "stopped", h.sd.stopped, []string{"ci-runner@0.service"})
}

func TestAnAdoptedRunnerIsStoppedOnlyIfGitHubLetsGo(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 0)}, set("a", "p"))
	if err := saveState(h.c.stateDir, &state{Runners: []record{
		{Pool: "p", Slot: 0, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-0-busy", ID: 10},
		{Pool: "p", Slot: 1, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-1-idle", ID: 11},
	}}); err != nil {
		t.Fatal(err)
	}
	h.sd.set("ci-runner@0.service", "active")
	h.sd.set("ci-runner@1.service", "active")
	if err := h.c.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	// After the restart, GitHub says one job runs, on slot 0's runner.
	// Which one has it isn't known until GitHub refuses a removal.
	h.c.byName["a"].client = &refusingClient{fakeClient: h.clients["a"], busy: 10}
	h.demand("a", 1, 1)
	h.now = h.at(idleGrace)

	// Slot 0's is tried first, refused, and kept as busy.
	h.step()
	equal(t, "stopped", h.sd.stopped, nil)
	if r := h.c.runners["ci-runner@0.service"]; r == nil || !r.busy || r.adopted {
		t.Errorf("the refused runner: %+v", r)
	}
	// Slot 1's is the surplus.
	h.step()
	equal(t, "stopped", h.sd.stopped, []string{"ci-runner@1.service"})
	equal(t, "removed", h.clients["a"].removals(), []string{"11/host-p-1-idle"})
}

// refusingClient refuses to remove the runner with ID busy, as GitHub
// refuses one mid-job.
type refusingClient struct {
	*fakeClient
	busy int64
}

func (c *refusingClient) RemoveRunner(ctx context.Context, id int64, name string) error {
	if id == c.busy {
		return actions.ErrJobStillRunning
	}
	return c.fakeClient.RemoveRunner(ctx, id, name)
}

// removedNames are the names of the registrations removed, sorted: runners
// registered concurrently get their IDs in either order.
func removedNames(c *fakeClient) []string {
	var out []string
	for _, r := range c.removals() {
		_, name, _ := strings.Cut(r, "/")
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
