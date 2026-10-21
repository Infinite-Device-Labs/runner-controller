package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

// TestRecoverAfterACrash sets up what a crash or a reboot leaves behind and
// checks what the next run makes of it.
func TestRecoverAfterACrash(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	dir := h.c.pools["p"].JITConfigDir

	if err := saveState(h.c.stateDir, &state{
		Runners: []record{
			// Still running: adopted.
			{Pool: "p", Slot: 0, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-0-old", ID: 10},
			// Gone with the reboot: its registration is removed.
			{Pool: "p", Slot: 1, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-1-old", ID: 11},
			// Failed: reset, and its registration removed.
			{Pool: "p", Slot: 2, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-2-old", ID: 12},
			// Registered, but the crash came before its ID was saved.
			{Pool: "p", Slot: 3, ScaleSet: "a", ScaleSetID: 100, Name: "host-p-3-old"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.sd.set("ci-runner@0.service", "active")
	h.sd.set("ci-runner@2.service", "failed")
	// A unit no record mentions is running.
	h.sd.set("ci-runner@5.service", "active")
	// Not a slot: left alone.
	h.sd.set("ci-runner@manual.service", "active")

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"0.env", "1.env", "2.env", ".3.env.tmp", "5.env", "README", "notes.env"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.c.recover(context.Background()); err != nil {
		t.Fatal(err)
	}

	var running []string
	for unit, r := range h.c.runners {
		running = append(running, unit)
		if unit == "ci-runner@0.service" && (r.foreign || r.ID != 10) {
			t.Errorf("the recorded unit wasn't adopted: %+v", r)
		}
		if unit == "ci-runner@5.service" && !r.foreign {
			t.Errorf("the unrecorded unit isn't marked foreign: %+v", r)
		}
	}
	slices.Sort(running)
	equal(t, "running", running, []string{"ci-runner@0.service", "ci-runner@5.service"})
	equal(t, "reset", h.sd.reset, []string{"ci-runner@2.service"})

	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	equal(t, "files left", left, []string{"0.env", "5.env", "README", "notes.env"})

	h.step()
	removed := h.clients["a"].removals()
	slices.Sort(removed)
	equal(t, "removed", removed, []string{"0/host-p-3-old", "11/host-p-1-old", "12/host-p-2-old"})

	// The adopted and the unrecorded units hold 4G of the 10G budget.
	h.demand("a", 5, 0)
	equal(t, "started", h.step(), []string{"ci-runner@1.service", "ci-runner@2.service", "ci-runner@3.service"})

	// When the unrecorded unit exits it is cleaned up, with no
	// registration to remove.
	h.sd.set("ci-runner@5.service", "inactive")
	h.step()
	if _, err := os.Stat(filepath.Join(dir, "5.env")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the unrecorded unit's file is still there: %v", err)
	}
	if got := len(h.clients["a"].removals()); got != 3 {
		t.Errorf("%d registrations removed, want still 3", got)
	}
}

func TestRecoverKeepsRemovalsFromTheLastRun(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	if err := saveState(h.c.stateDir, &state{Removals: []*removal{{ScaleSet: "a", RunnerID: 7, Name: "host-p-0-x"}}}); err != nil {
		t.Fatal(err)
	}
	if err := h.c.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.step()
	equal(t, "removed", h.clients["a"].removals(), []string{"7/host-p-0-x"})
}

func TestRecoverSetsAnUnreadableStateFileAside(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	if err := os.MkdirAll(h.c.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.c.stateDir, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.sd.set("ci-runner@0.service", "active")

	if err := h.c.recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.c.stateDir, stateFile+".unreadable")); err != nil {
		t.Errorf("the unreadable file wasn't set aside: %v", err)
	}
	if r := h.c.runners["ci-runner@0.service"]; r == nil || !r.foreign {
		t.Errorf("the running unit isn't counted: %+v", r)
	}
}
