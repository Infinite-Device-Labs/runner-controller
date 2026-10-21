package controller

import (
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

// dieMidJob starts one runner for one job, has the job start on it, then
// kills its unit while GitHub still counts the job as running.
func dieMidJob(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	equal(t, "started", h.step(), []string{"ci-runner@0.service"})
	h.jobStarts("a", 1, 1)
	h.clients["a"].removeErr = actions.ErrJobStillRunning
	h.sd.set("ci-runner@0.service", "failed")
	return h
}

func TestADeadRunnersJobIsntReplaced(t *testing.T) {
	h := dieMidJob(t)
	equal(t, "started for the orphaned job", h.step(), nil)
	if len(h.c.removals) != 1 || !h.c.removals[0].Orphan {
		t.Fatalf("removals %+v, want one orphan", h.c.removals)
	}
	equal(t, "started later", h.step(), nil)

	// A new job still gets a runner.
	h.demand("a", 2, 1, h.at(time.Minute))
	equal(t, "started for a new job", h.step(), []string{"ci-runner@0.service"})
}

func TestAnOrphanIsRemovedAsSoonAsGitHubLetsGo(t *testing.T) {
	h := dieMidJob(t)
	h.step()
	if h.c.removals[0].Next.Sub(h.now) != removalRetry {
		t.Fatalf("first retry at +%v", h.c.removals[0].Next.Sub(h.now))
	}

	// GitHub fails the job well before the retry is due.
	h.now = h.at(10 * time.Second)
	h.clients["a"].removeErr = nil
	h.demand("a", 0, 0)
	h.step()
	equal(t, "removed", h.clients["a"].removals(), []string{"1/host-p-0-r1"})
	if len(h.c.removals) != 0 {
		t.Errorf("removals left: %+v", h.c.removals)
	}
}

func TestAnOrphansRetriesAreCapped(t *testing.T) {
	h := dieMidJob(t)
	for i := range 10 {
		h.step()
		rm := h.c.removals[0]
		if gap := rm.Next.Sub(h.now); gap > orphanRetryUp {
			t.Fatalf("attempt %d: retry gap %v, over the %v cap", i+1, gap, orphanRetryUp)
		}
		h.now = rm.Next
	}
}

func TestAnIdleRunnerKeptOnlyForAnOrphanIsStopped(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	// A runner started for a job before its orphan was known: the only
	// assigned job is the orphan, so the runner is surplus.
	h.demand("a", 1, 1, h.at(0))
	h.step()
	h.c.removals = append(h.c.removals, &removal{ScaleSet: "a", RunnerID: 99, Name: "host-p-9-dead", Orphan: true, Next: h.at(time.Hour)})
	h.now = h.at(idleGrace)
	h.step()
	equal(t, "stopped", h.sd.stopped, []string{"ci-runner@0.service"})
}

func TestAnOrphanSurvivesARestart(t *testing.T) {
	h := dieMidJob(t)
	h.step()
	h2 := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h2.c.stateDir = h.c.stateDir
	if err := h2.c.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h2.c.removals) != 1 || !h2.c.removals[0].Orphan {
		t.Fatalf("removals after the restart %+v, want the orphan", h2.c.removals)
	}
	h2.demand("a", 1, 1)
	equal(t, "started for the orphaned job after the restart", h2.step(), nil)
}

func TestARunnerKilledBeforeItsStartedEventIsntAFailure(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()
	// The job started, but its event is still a long poll away when the
	// unit dies. GitHub's refusal shows the runner had a job.
	h.clients["a"].removeErr = actions.ErrJobStillRunning
	h.sd.set("ci-runner@0.service", "failed")
	h.demand("a", 1, 1)
	equal(t, "started", h.step(), nil)
	if s := h.c.byName["a"]; s.failures != 0 || !s.retryAt.IsZero() {
		t.Errorf("counted as a failure: failures %d, retry at %v", s.failures, s.retryAt)
	}
	if len(h.c.removals) != 1 || !h.c.removals[0].Orphan || h.c.removals[0].IdleExit {
		t.Errorf("removals %+v, want one orphan, decided", h.c.removals)
	}
}
