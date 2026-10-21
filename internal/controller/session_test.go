package controller

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

func TestSessionAcknowledgesAcquiresAndRecords(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	queued := h.at(-time.Minute)
	sess := &fakeSession{
		stats: actions.Statistics{AssignedJobs: 1},
		messages: []*actions.Message{
			{
				ID:         7,
				Statistics: actions.Statistics{AssignedJobs: 2},
				Available:  []actions.Job{{RequestID: 1, QueueTime: queued}, {RequestID: 2}},
			},
			nil, // the long poll ran out
			{
				ID:         8,
				Statistics: actions.Statistics{AssignedJobs: 2, RunningJobs: 1},
				Started:    []actions.Job{{RequestID: 2}},
			},
		},
	}
	h.clients["a"].session = sess
	s := h.c.byName["a"]

	if err := h.c.session(context.Background(), s); err == nil || err.Error() != "script over" {
		t.Fatalf("session ended with %v", err)
	}
	equal(t, "acknowledged", sess.deleted, []int{7, 8})
	equal(t, "acquired", sess.acquired, []int64{1, 2})
	equal(t, "last message IDs", sess.lastIDs, []int{0, 7, 7, 8})
	if !sess.closed {
		t.Error("the session wasn't closed")
	}
	if s.assigned != 2 {
		t.Errorf("assigned %d, want 2", s.assigned)
	}
	if len(s.pending) != 1 || !s.pending[1].Equal(queued) {
		t.Errorf("pending %v, want job 1 queued at %v", s.pending, queued)
	}
}

func TestPendingIsClearedWhenNothingWaits(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.demand("a", 2, 0, h.at(0), h.at(1))
	// The started events were lost to a truncated message; the statistics
	// say nothing waits.
	h.demand("a", 2, 2)
	if n := len(h.c.byName["a"].pending); n != 0 {
		t.Errorf("%d jobs still pending", n)
	}
}

func TestAFailedAcquireKeepsTheMessagesOtherEvents(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.demand("a", 1, 0, h.at(0))
	h.step()
	sess := &fakeSession{
		acquireErr: errors.New("acquire failed"),
		messages: []*actions.Message{{
			ID:         3,
			Statistics: actions.Statistics{AssignedJobs: 2, RunningJobs: 1},
			Available:  []actions.Job{{RequestID: 9}},
			Started:    []actions.Job{{RequestID: 1, RunnerName: "host-p-0-r1"}},
		}},
	}
	h.clients["a"].session = sess
	if err := h.c.session(context.Background(), h.c.byName["a"]); err == nil || !strings.Contains(err.Error(), "acquire failed") {
		t.Fatalf("session ended with %v", err)
	}
	equal(t, "acknowledged", sess.deleted, []int{3})
	if !h.c.runners["ci-runner@0.service"].busy {
		t.Error("the started event was lost")
	}
	if h.c.byName["a"].assigned != 2 {
		t.Error("the statistics were lost")
	}
}

func TestAnEmptyPollThatReturnsAtOnceWaits(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.c.emptyPollWait = time.Hour
	h.clients["a"].session = &fakeSession{messages: []*actions.Message{nil, nil}}
	h.c.session(context.Background(), h.c.byName["a"])
	slept := h.sleeps()
	if len(slept) != 2 || slept[0] < 59*time.Minute {
		t.Errorf("slept %v, want two waits of about an hour", slept)
	}
}

func TestListenBacksOffAndResetsAfterAHealthySession(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	fc := h.clients["a"]
	refused := errors.New("a session already exists")
	fc.openErrs = []error{refused, refused, refused, refused, refused, refused, refused}

	ctx, cancel := context.WithCancel(context.Background())
	// The eighth open succeeds. Its session lasts long enough to count as
	// healthy, then fails: the backoff starts again at 5s. The ninth open is
	// refused, the second failure since, and the controller is stopped
	// while it waits.
	sess := &fakeSession{}
	sess.onGet = func() { h.now = h.now.Add(sessionHealthy + time.Second) }
	fc.session = sess
	h.c.sleep = func(ctx context.Context, d time.Duration) {
		h.sleptMu.Lock()
		h.slept = append(h.slept, d)
		n := len(h.slept)
		h.sleptMu.Unlock()
		if n == 8 {
			fc.mu.Lock()
			fc.openErrs = []error{refused}
			fc.mu.Unlock()
		}
		if n == 9 {
			cancel()
		}
	}
	h.c.listen(ctx, h.c.byName["a"])

	s := time.Second
	equal(t, "waits", h.sleeps(), []time.Duration{5 * s, 10 * s, 20 * s, 40 * s, 80 * s, 120 * s, 120 * s, 5 * s, 10 * s})
	if !sess.closed {
		t.Error("the session wasn't closed")
	}
}

func TestASessionAcquiresJobsOfferedWhileNoneWasOpen(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	queued := h.at(-10 * time.Minute)
	fc := h.clients["a"]
	fc.acquirable = []actions.Job{{RequestID: 7, QueueTime: queued}}
	sess := &fakeSession{stats: actions.Statistics{AvailableJobs: 1}}
	fc.session = sess

	h.c.session(context.Background(), h.c.byName["a"])
	equal(t, "acquired", sess.acquired, []int64{7})
	if got := h.c.byName["a"].pending[7]; !got.Equal(queued) {
		t.Errorf("job 7 pending since %v, want %v", got, queued)
	}
}

func TestASessionWithNothingWaitingDoesntAsk(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	h.clients["a"].session = &fakeSession{}
	h.c.session(context.Background(), h.c.byName["a"])
	if n := h.clients["a"].acquirableAsks; n != 0 {
		t.Errorf("asked for acquirable jobs %d times with none available", n)
	}
}

func TestASessionCarriesOnIfListingLeftoversFails(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	fc := h.clients["a"]
	fc.acquirableErr = errors.New("404 Not Found")
	sess := &fakeSession{stats: actions.Statistics{AvailableJobs: 1}}
	fc.session = sess

	h.c.session(context.Background(), h.c.byName["a"])
	if len(sess.lastIDs) == 0 {
		t.Error("the session stopped instead of polling")
	}
	equal(t, "acquired", sess.acquired, nil)
}

func TestDebugLoggingShowsJobEventsAndAcquirableListings(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	var out bytes.Buffer
	h.c.log = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.clients["a"].session = &fakeSession{
		stats: actions.Statistics{AssignedJobs: 1},
		messages: []*actions.Message{{
			ID:         4,
			Statistics: actions.Statistics{AssignedJobs: 1, RunningJobs: 1},
			Started:    []actions.Job{{RequestID: 9, RunnerName: "host-p-0-ab"}},
		}},
	}
	h.c.session(context.Background(), h.c.byName["a"])

	for _, want := range []string{
		`msg="listed acquirable jobs" scale_set=a jobs=0`,
		`msg=message scale_set=a id=4 available_jobs=0 assigned_jobs=1 running_jobs=1`,
		`msg="job event" scale_set=a event=started request_id=9 runner=host-p-0-ab`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}

	// At info level, none of it.
	out.Reset()
	h.c.log = slog.New(slog.NewTextHandler(&out, nil))
	h.c.observe(h.c.byName["a"], actions.Statistics{}, &actions.Message{Started: []actions.Job{{RequestID: 10}}})
	if strings.Contains(out.String(), "job event") {
		t.Errorf("job events logged at info level:\n%s", out.String())
	}
}
