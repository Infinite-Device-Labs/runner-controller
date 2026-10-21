package controller

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

// TestConcurrentStartsAndReleasesKeepTheBooks runs reconcile passes while
// earlier passes' starts are still registering or starting, some of them
// failing, and units exit at random. Run with -race, it checks the locking; after each
// pass and at the end it checks that the budget held and that the JIT files
// on disk are exactly those of the started runners.
func TestConcurrentStartsAndReleasesKeepTheBooks(t *testing.T) {
	h := newHarness(t, "8G", map[string]*config.Pool{"p": pool("ci-runner", "1G", 0, 0), "q": pool("runner-q", "2G", 0, 0)},
		set("a", "p"), set("b", "q"))
	var clock atomic.Int64
	clock.Store(h.now.UnixNano())
	h.c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	for _, fc := range h.clients {
		fc.jitHook = func(name string) error {
			time.Sleep(time.Duration(rand.IntN(200)) * time.Microsecond)
			if rand.IntN(3) == 0 {
				return errors.New("registration refused")
			}
			return nil
		}
	}

	// A failed start is released from its own goroutine after its file was
	// written, while a reconcile pass may be reusing the slot.
	h.sd.startHook = func(unit string) error {
		if rand.IntN(4) == 0 {
			return errors.New("start failed")
		}
		return nil
	}

	ctx := context.Background()
	for range 300 {
		// Past any backoff, so failed scale sets keep trying.
		clock.Add(int64(time.Hour))
		h.demand("a", rand.IntN(10), 0)
		h.demand("b", rand.IntN(5), 0)
		h.sd.mu.Lock()
		for unit, state := range h.sd.states {
			if state == "active" && rand.IntN(4) == 0 {
				h.sd.states[unit] = "inactive"
			}
		}
		h.sd.mu.Unlock()
		h.c.reconcile(ctx)
		checkBudget(t, h)
		checkRunningUnitsHaveTheirFiles(t, h)
	}
	h.c.starts.Wait()
	h.c.reconcile(ctx)
	h.c.starts.Wait()
	checkBudget(t, h)

	for name, p := range h.c.pools {
		entries, err := os.ReadDir(p.JITConfigDir)
		if err != nil {
			t.Fatal(err)
		}
		files := map[string]bool{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("pool %s: a temporary file was left: %s", name, e.Name())
				continue
			}
			files[e.Name()] = true
		}
		for unit, r := range h.c.runners {
			if r.Pool != name {
				continue
			}
			f := strconv.Itoa(r.Slot) + ".env"
			if !files[f] {
				t.Errorf("started runner %s has no JIT file", unit)
			}
			delete(files, f)
		}
		for f := range files {
			t.Errorf("pool %s: %s belongs to no runner", name, filepath.Join(p.JITConfigDir, f))
		}
	}
}

func checkBudget(t *testing.T, h *harness) {
	t.Helper()
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	var used int64
	for _, r := range h.c.runners {
		used += int64(h.c.pools[r.Pool].Memory)
	}
	if used > h.c.budget {
		t.Fatalf("the runners declare %s of a %s budget", config.ByteSize(used), config.ByteSize(h.c.budget))
	}
}

// checkRunningUnitsHaveTheirFiles fails if a started runner's unit is up
// while its JIT file is gone: a release that freed the slot before removing
// its own file would have removed the next runner's.
func checkRunningUnitsHaveTheirFiles(t *testing.T, h *harness) {
	t.Helper()
	h.c.mu.Lock()
	var running []*runner
	for _, r := range h.c.runners {
		if r.started {
			running = append(running, r)
		}
	}
	h.c.mu.Unlock()
	for _, r := range running {
		h.sd.mu.Lock()
		state := h.sd.state(r.unit)
		h.sd.mu.Unlock()
		if state != "active" {
			continue
		}
		path := filepath.Join(h.c.pools[r.Pool].JITConfigDir, strconv.Itoa(r.Slot)+".env")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s is running without its JIT file: %v", r.unit, err)
		}
	}
}
