package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
)

const (
	// idleGrace is the minimum runtime before a runner can be surplus;
	// statistics lag events, and without it runners would churn.
	idleGrace = time.Minute
	// scaleDownRetry delays a retry after a failed removal or stop.
	scaleDownRetry = time.Minute
)

// scaleDown stops idle runners beyond a scale set's min_runners plus its
// assigned jobs, less orphans, returning their memory to the budget.
//
// GitHub may hand a job to a runner just as it is chosen, so the
// registration is removed first and the unit stopped only once that
// succeeds; GitHub refuses to remove a runner that has taken a job. Adopted
// units, whose job is unknown, are probed the same way.
func (c *Controller) scaleDown(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	now := c.now()
	orphans := c.orphans()
	var chosen []*runner
	var stops []*runner
	for _, s := range c.sets {
		var live int
		var idle []*runner
		for _, r := range c.runners {
			if r.ScaleSet != s.cfg.Name || r.foreign || r.done {
				continue
			}
			if r.deregistered {
				if !r.stopRequested && !now.Before(r.nextScaleDown) {
					stops = append(stops, r)
				}
				continue
			}
			live++
			if r.started && !r.stopping && (!r.busy || r.adopted) &&
				now.Sub(r.startedAt) >= idleGrace && !now.Before(r.nextScaleDown) {
				idle = append(idle, r)
			}
		}
		surplus := live - (s.pool.MinRunners + max(s.assigned-orphans[s.cfg.Name], 0))
		if surplus <= 0 || len(idle) == 0 {
			continue
		}
		// Longest-running first, then by unit for a stable order.
		slices.SortFunc(idle, func(a, b *runner) int {
			if c := a.startedAt.Compare(b.startedAt); c != 0 {
				return c
			}
			return strings.Compare(a.unit, b.unit)
		})
		for _, r := range idle[:min(surplus, len(idle))] {
			r.stopping = true
			chosen = append(chosen, r)
		}
	}
	c.mu.Unlock()

	for _, r := range chosen {
		s := c.byName[r.ScaleSet]
		err := s.client.RemoveRunner(ctx, r.ID, r.Name)
		c.mu.Lock()
		r.stopping = false
		switch {
		case errors.Is(err, actions.ErrJobStillRunning):
			// It took a job.
			r.busy, r.adopted = true, false
			c.log.Info("an idle runner took a job as it was being stopped; leaving it", "unit", r.unit, "scale_set", r.ScaleSet)
		case err != nil:
			r.nextScaleDown = c.now().Add(scaleDownRetry)
			c.log.Warn("couldn't remove an idle runner's registration; will retry", "unit", r.unit, "scale_set", r.ScaleSet, "error", err)
		default:
			r.deregistered = true
			stops = append(stops, r)
			c.log.Info("stopping an idle runner no job needs", "unit", r.unit, "scale_set", r.ScaleSet, "runner", r.Name)
		}
		c.mu.Unlock()
	}

	for _, r := range stops {
		err := c.sd.Stop(ctx, r.unit)
		c.mu.Lock()
		if err != nil {
			r.nextScaleDown = c.now().Add(scaleDownRetry)
			c.log.Warn("couldn't stop an idle runner's unit; will retry", "unit", r.unit, "error", err)
		} else {
			r.stopRequested = true
		}
		c.mu.Unlock()
	}
	if len(chosen) > 0 {
		c.signal()
	}
}
