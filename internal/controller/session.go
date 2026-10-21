package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
)

const (
	sessionRetryUp = 2 * time.Minute
	// sessionHealthy is how long a session must have lasted for the next
	// failure to start its backoff afresh.
	sessionHealthy = 2 * time.Minute
)

// listen holds a scale set's message session until ctx ends, reopening it
// after a failure. Another session for the scale set, such as one left by a
// crashed controller, makes opening fail until GitHub expires it.
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
	c.log.Info("message session open", "scale_set", s.cfg.Name)
	c.observe(s, sess.Statistics(), nil)

	last := 0
	for {
		polled := time.Now()
		msg, err := sess.GetMessage(ctx, last, c.maxCapacity(s))
		if err != nil {
			return err
		}
		if msg == nil {
			// GitHub holds an empty poll open for about 50 seconds. One
			// that comes back at once mustn't turn into a busy loop.
			if wait := c.emptyPollWait - time.Since(polled); wait > 0 {
				c.sleep(ctx, wait)
			}
			continue
		}
		last = msg.ID
		// Acknowledged first, as GitHub's own listener does: the statistics
		// in the next message correct anything this one's handling misses.
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
		// The message is acknowledged, so its other events are recorded
		// even if acquiring failed: they won't come again.
		c.observe(s, msg.Statistics, msg)
		if acquireErr != nil {
			return acquireErr
		}
	}
}

// observe records a scale set's statistics and a message's job events, and
// wakes the loop to act on them.
func (c *Controller) observe(s *scaleSet, st actions.Statistics, msg *actions.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if msg != nil {
		for _, jobs := range [][]actions.Job{msg.Available, msg.Assigned} {
			for _, j := range jobs {
				if _, ok := s.pending[j.RequestID]; ok {
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
	s.assigned = st.AssignedJobs
	// Nothing waiting: whatever is still pending was missed leaving.
	if st.AssignedJobs <= st.RunningJobs {
		clear(s.pending)
	}
	c.signal()
}

// ranJob marks the named runner busy, and done once its job completed: it
// has run a job, so its scale set's runners start and work, and its
// failures are forgotten. It is called with c.mu held.
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
