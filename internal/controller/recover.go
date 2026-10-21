package controller

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/Infinite-Device-Labs/runner-controller/internal/systemd"
)

// recover reconciles the state file with what systemd has now, after a
// restart, crash or reboot:
//
//   - a recorded unit still running is adopted, and its memory counted;
//   - a recorded unit that has gone has its registration queued for
//     removal;
//   - a running unit of a pool's template with no record is counted against
//     the budget until it exits, but otherwise left alone;
//   - failed units are reset, and JIT config files of free slots removed.
func (c *Controller) recover(ctx context.Context) error {
	c.mu.Lock()
	err := c.recoverLocked(ctx)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	c.save()
	return nil
}

func (c *Controller) recoverLocked(ctx context.Context) error {
	st, err := loadState(c.stateDir)
	if err != nil {
		// Set an unreadable file aside rather than refuse to start; systemd
		// still knows which units are running.
		aside := filepath.Join(c.stateDir, stateFile+".unreadable")
		c.log.Error("the state file can't be read; setting it aside and recovering from systemd alone", "moved_to", aside, "error", err)
		if err := os.Rename(filepath.Join(c.stateDir, stateFile), aside); err != nil {
			return fmt.Errorf("setting the unreadable state file aside: %w", err)
		}
		st = &state{}
	}

	recorded := map[string]record{}
	for _, rec := range st.Runners {
		if p := c.pools[rec.Pool]; p != nil {
			recorded[unitName(p, rec.Slot)] = rec
		} else {
			recorded[rec.Pool+"@"+fmt.Sprint(rec.Slot)] = rec
		}
	}
	c.removals = st.Removals

	for _, name := range slices.Sorted(maps.Keys(c.pools)) {
		p := c.pools[name]
		units, err := c.sd.List(ctx, p.Unit+"@*.service")
		if err != nil {
			return fmt.Errorf("listing pool %q's units: %w", name, err)
		}
		for _, unit := range slices.Sorted(maps.Keys(units)) {
			state := units[unit]
			slot, ok := slotOf(p, unit)
			if !ok {
				continue
			}
			rec, known := recorded[unit]
			if systemd.Exited(state) {
				if state == "failed" {
					if err := c.sd.ResetFailed(ctx, unit); err != nil {
						c.log.Warn("couldn't reset a failed unit", "unit", unit, "error", err)
					}
				}
				continue
			}
			delete(recorded, unit)
			if known && c.byName[rec.ScaleSet] != nil {
				// Its job is unknown. Counting it busy only makes its scale
				// set's waiting jobs look older; scale-down probes it.
				c.runners[unit] = &runner{record: rec, unit: unit, started: true, startedAt: c.now(), busy: true, adopted: true}
				c.log.Info("adopted a running unit", "unit", unit, "scale_set", rec.ScaleSet, "runner", rec.Name)
				continue
			}
			c.runners[unit] = &runner{record: record{Pool: name, Slot: slot}, unit: unit, started: true, foreign: true}
			c.log.Warn("a unit with no record is running; counting it against the budget until it exits", "unit", unit)
		}
	}

	now := c.now()
	for _, unit := range slices.Sorted(maps.Keys(recorded)) {
		rec := recorded[unit]
		c.log.Info("cleaning up a runner left from before", "unit", unit, "runner", rec.Name)
		c.removals = append(c.removals, &removal{ScaleSet: rec.ScaleSet, RunnerID: rec.ID, Name: rec.Name, Since: now, Next: now})
	}

	for _, name := range slices.Sorted(maps.Keys(c.pools)) {
		p := c.pools[name]
		files, err := leftoverJITFiles(p.JITConfigDir, func(slot int) bool {
			return c.runners[unitName(p, slot)] != nil
		})
		if err != nil {
			return fmt.Errorf("reading pool %q's JIT config directory: %w", name, err)
		}
		for _, f := range files {
			if err := os.Remove(f); err != nil {
				return fmt.Errorf("removing a leftover JIT config file: %w", err)
			}
			c.log.Info("removed a leftover JIT config file", "file", f)
		}
	}
	return nil
}
