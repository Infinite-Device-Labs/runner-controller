package controller

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
)

const (
	sessionRetryUp = 2 * time.Minute
	// sessionHealthy: a session that lasted this long resets the backoff.
	sessionHealthy = 2 * time.Minute
)

// listen holds s's message session until ctx ends, reopening it with backoff.
// A session left open by a crashed controller blocks a new one until GitHub
// expires it.
func (c *Controller) listen(ctx context.Context, s *scaleSet) {
	log := c.log.With("scale_set", s.cfg.Name)
	failures := 0
	for ctx.Err() == nil {
		opened := c.now()
		err := c.session(ctx, s)
		if ctx.Err() != nil {
			return
		}
		if c.now().Sub(opened) > sessionHealthy {
			failures = 0
		}
		failures++
		wait := backoff(c.sessionRetry, sessionRetryUp, failures)
		log.Warn("the message session failed; reopening it", "in", wait, "error", err)
		c.sleep(ctx, wait)
	}
}

// session opens one message session and handles its messages until it fails
// or ctx ends.
func (c *Controller) session(ctx context.Context, s *scaleSet) error {
	sess, err := s.client.OpenSession(ctx, s.id, c.owner)
	if err != nil {
		return fmt.Errorf("opening the session: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := sess.Close(ctx); err != nil {
			c.log.Warn("couldn't close the message session", "scale_set", s.cfg.Name, "error", err)
		}
	}()
	st := sess.Statistics()
	c.log.Info("message session open", "scale_set", s.cfg.Name, "available_jobs", st.AvailableJobs, "assigned_jobs", st.AssignedJobs)
	c.observe(s, st, nil)
	if err := c.acquireLeftovers(ctx, s, sess, st); err != nil {
		c.log.Warn("couldn't acquire jobs offered while no session was open; they wait for the scale set's next message",
			"scale_set", s.cfg.Name, "available_jobs", st.AvailableJobs, "error", err)
	}

	last := 0
	for {
		polled := time.Now()
		msg, err := sess.GetMessage(ctx, last, c.maxCapacity(s))
		if err != nil {
			return err
		}
		if msg == nil {
			// Empty polls normally take ~50s; don't spin if one returns
			// at once.
			if wait := c.emptyPollWait - time.Since(polled); wait > 0 {
				c.sleep(ctx, wait)
			}
			continue
		}
		last = msg.ID
		c.log.Debug("message", "scale_set", s.cfg.Name, "id", msg.ID,
			"available_jobs", msg.Statistics.AvailableJobs, "assigned_jobs", msg.Statistics.AssignedJobs, "running_jobs", msg.Statistics.RunningJobs)
		// Ack first, as the upstream listener does; the next message's
		// statistics correct anything this one's handling misses.
		if err := sess.DeleteMessage(ctx, msg.ID); err != nil {
			return fmt.Errorf("acknowledging message %d: %w", msg.ID, err)
		}
		var acquireErr error
		if len(msg.Available) > 0 {
			ids := make([]int64, len(msg.Available))
			for i, j := range msg.Available {
				ids[i] = j.RequestID
			}
			if err := sess.AcquireJobs(ctx, ids); err != nil {
				acquireErr = fmt.Errorf("acquiring %d jobs: %w", len(ids), err)
			}
		}
		// Record the events even if acquiring failed: the message is acked
		// and won't be redelivered.
		c.observe(s, msg.Statistics, msg)
		if acquireErr != nil {
			return acquireErr
		}
	}
}

// acquireLeftovers acquires jobs offered while no session was open. GitHub
// offers them again only with the scale set's next message, which on a quiet
// scale set can be hours away. The condition is actions-runner-controller's.
func (c *Controller) acquireLeftovers(ctx context.Context, s *scaleSet, sess actions.Session, st actions.Statistics) error {
	if st.AvailableJobs == 0 && st.AssignedJobs == 0 {
		return nil
	}
	jobs, err := s.client.AcquirableJobs(ctx, s.id)
	if err != nil {
		return err
	}
	c.log.Debug("listed acquirable jobs", "scale_set", s.cfg.Name, "jobs", len(jobs))
	if len(jobs) == 0 {
		return nil
	}
	var ids []int64
	for _, j := range jobs {
		if j.RequestID != 0 {
			ids = append(ids, j.RequestID)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("%d acquirable jobs listed, none with a request ID (#12)", len(jobs))
	}
	if err := sess.AcquireJobs(ctx, ids); err != nil {
		return err
	}
	c.log.Info("acquired jobs offered while no session was open", "scale_set", s.cfg.Name, "jobs", len(ids))
	c.observe(s, st, &actions.Message{Available: jobs})
	return nil
}

// observe records a scale set's statistics and a message's job events, and
// wakes the loop to act on them.
func (c *Controller) observe(s *scaleSet, st actions.Statistics, msg *actions.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if msg != nil {
		c.logEvents(s, msg)
		for _, jobs := range [][]actions.Job{msg.Available, msg.Assigned} {
			for _, j := range jobs {
				// A zero ID would collapse every job into one entry (#12).
				if _, ok := s.pending[j.RequestID]; ok || j.RequestID == 0 {
					continue
				}
				t := j.QueueTime
				if t.IsZero() {
					t = now
				}
				s.pending[j.RequestID] = t
			}
		}
		for _, j := range msg.Started {
			delete(s.pending, j.RequestID)
			c.ranJob(j.RunnerName, false)
		}
		for _, j := range msg.Completed {
			delete(s.pending, j.RequestID)
			c.ranJob(j.RunnerName, true)
		}
	}
	// Fewer jobs counted may mean GitHub has failed an orphan's job: retry
	// its removal now rather than at its next backoff.
	if st.AssignedJobs < s.assigned || st.RunningJobs < s.running {
		for _, rm := range c.removals {
			if rm.Orphan && rm.ScaleSet == s.cfg.Name {
				rm.Next = now
			}
		}
	}
	s.assigned, s.running = st.AssignedJobs, st.RunningJobs
	// Nothing waiting: anything still pending missed its started event.
	if st.AssignedJobs <= st.RunningJobs && st.AvailableJobs == 0 {
		clear(s.pending)
	}
	c.signal()
}

// logEvents logs each job event at debug level. Messages hold at most 50.
func (c *Controller) logEvents(s *scaleSet, msg *actions.Message) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	for _, e := range []struct {
		kind string
		jobs []actions.Job
	}{{"available", msg.Available}, {"assigned", msg.Assigned}, {"started", msg.Started}, {"completed", msg.Completed}} {
		for _, j := range e.jobs {
			c.log.Debug("job event", "scale_set", s.cfg.Name, "event", e.kind, "request_id", j.RequestID,
				"runner", j.RunnerName, "queued", j.QueueTime, "result", j.Result)
		}
	}
}

// ranJob marks the named runner busy, and done if its job completed. A
// runner taking a job proves its scale set's runners work, so the failure
// count resets. Call with c.mu held.
func (c *Controller) ranJob(name string, done bool) {
	if name == "" {
		return
	}
	for _, r := range c.runners {
		if r.Name == name {
			r.busy = true
			r.adopted = false
			r.done = r.done || done
			if s := c.byName[r.ScaleSet]; s != nil {
				s.failures = 0
			}
		}
	}
}
