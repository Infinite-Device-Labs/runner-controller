package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

// stateFile, in the state directory, records started runners and pending
// registration removals.
const stateFile = "runners.json"

type state struct {
	Runners  []record   `json:"runners"`
	Removals []*removal `json:"removals"`
}

// record is a started runner as persisted across restarts.
type record struct {
	Pool       string `json:"pool"`
	Slot       int    `json:"slot"`
	ScaleSet   string `json:"scale_set"`
	ScaleSetID int    `json:"scale_set_id"`
	// Name is chosen before registering, so a registration whose ID never
	// came back can still be found and removed.
	Name string `json:"name"`
	// ID is set once GitHub returns it.
	ID int64 `json:"id,omitempty"`
}

// removal is a registration still to remove after its unit has gone.
type removal struct {
	ScaleSet string    `json:"scale_set"`
	RunnerID int64     `json:"runner_id,omitempty"`
	Name     string    `json:"name"`
	Since    time.Time `json:"since"`
	Attempts int       `json:"attempts,omitempty"`
	Next     time.Time `json:"next"`
	// Orphan: the last attempt was refused because GitHub still counts the
	// runner's job as running. That job is in the scale set's assigned
	// count but no runner can take it, so it isn't demand.
	Orphan bool `json:"orphan,omitempty"`
	// IdleExit: the runner exited with no job event seen. JobStarted can lag
	// by a long poll, so the first removal attempt decides: a JobStillRunning
	// refusal shows it had a job; anything else counts a failure against its
	// scale set.
	IdleExit bool `json:"idle_exit,omitempty"`
}

func loadState(dir string) (*state, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return &state{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", filepath.Join(dir, stateFile), err)
	}
	return &st, nil
}

func saveState(dir string, st *state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, stateFile, append(b, '\n'))
}

// writeFileAtomic writes dir/name with mode 0600 via a temp file and rename.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func jitFileName(slot int) string {
	return strconv.Itoa(slot) + ".env"
}

// writeJITFile writes a slot's config as an env file, for systemd's
// EnvironmentFile= or podman's --env-file. The runner reads
// ACTIONS_RUNNER_INPUT_JITCONFIG as its --jitconfig and scrubs it from the
// environment its jobs inherit.
func writeJITFile(dir string, slot int, encoded string) error {
	return writeFileAtomic(dir, jitFileName(slot), []byte("ACTIONS_RUNNER_INPUT_JITCONFIG="+encoded+"\n"))
}

func removeJITFile(dir string, slot int) error {
	err := os.Remove(filepath.Join(dir, jitFileName(slot)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// jitFile matches a slot's file and its temp file; nothing else in the
// directory is touched.
var jitFile = regexp.MustCompile(`^\.?([0-9]+)\.env(\.tmp)?$`)

// leftoverJITFiles lists dir's JIT files for slots inUse reports free.
func leftoverJITFiles(dir string, inUse func(slot int) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		m := jitFile.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		slot, err := strconv.Atoi(m[1])
		if err != nil || inUse(slot) {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// privateDir requires dir to be a real directory owned by the controller's
// user and writable by no one else: write access to a JIT dir lets a user
// swap in their own runner config, and to the state dir lets them have the
// App remove any runner. With create, a missing dir is made 0700.
func privateDir(dir string, create bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link, not a directory", dir)
	case !info.IsDir():
		return fmt.Errorf("%s is not a directory", dir)
	case info.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%s can be written by users other than its owner (mode %04o); make it 0700", dir, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not by the controller's uid %d", dir, st.Uid, os.Geteuid())
	}
	return nil
}

// prepareDirs creates or checks the state and JIT config directories.
func prepareDirs(stateDir string, pools map[string]*config.Pool) error {
	return eachDir(stateDir, pools, func(dir string) error { return privateDir(dir, true) })
}

// CheckDirs checks cfg's existing directories as Run would, creating none.
func CheckDirs(cfg *config.Config) error {
	return eachDir(cfg.StateDir, cfg.Pools, func(dir string) error { return privateDir(dir, false) })
}

func eachDir(stateDir string, pools map[string]*config.Pool, check func(string) error) error {
	dirs := []string{stateDir}
	for _, p := range pools {
		dirs = append(dirs, p.JITConfigDir)
	}
	var errs []error
	for _, dir := range dirs {
		if err := check(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
