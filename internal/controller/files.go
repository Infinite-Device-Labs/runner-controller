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

// stateFile is the record, in the state directory, of the runners the
// controller has started and the registrations it still has to remove.
const stateFile = "runners.json"

type state struct {
	Runners  []record   `json:"runners"`
	Removals []*removal `json:"removals"`
}

// record is a runner the controller started, as it is kept across restarts.
type record struct {
	Pool       string `json:"pool"`
	Slot       int    `json:"slot"`
	ScaleSet   string `json:"scale_set"`
	ScaleSetID int    `json:"scale_set_id"`
	// Name is chosen before the runner is registered, so a registration
	// whose ID never came back can still be found and removed.
	Name string `json:"name"`
	// ID is the registration's, once GitHub has returned it.
	ID int64 `json:"id,omitempty"`
}

// removal is a registration still to be removed, once its unit has gone.
type removal struct {
	ScaleSet string    `json:"scale_set"`
	RunnerID int64     `json:"runner_id,omitempty"`
	Name     string    `json:"name"`
	Since    time.Time `json:"since"`
	Attempts int       `json:"attempts,omitempty"`
	Next     time.Time `json:"next"`
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

// writeFileAtomic writes dir/name, readable by its owner alone, so that a
// reader sees the old file or the new one and never half of one.
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

// jitFileName is the file a slot's unit reads its runner's configuration
// from.
func jitFileName(slot int) string {
	return strconv.Itoa(slot) + ".env"
}

// writeJITFile writes a slot's configuration as an environment file, which
// both systemd's EnvironmentFile= and podman's --env-file read. The runner
// takes ACTIONS_RUNNER_INPUT_JITCONFIG as its --jitconfig.
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

// jitFile matches a slot's configuration file and the temporary file it is
// written through. Nothing else in the directory is touched.
var jitFile = regexp.MustCompile(`^\.?([0-9]+)\.env(\.tmp)?$`)

// leftoverJITFiles lists the files in dir that belong to a slot inUse
// reports free.
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

// privateDir checks that dir is a directory, not a symbolic link, owned by
// the controller's user and writable by no one else. Anyone who could write
// to a pool's directory could swap a job's JIT config for their own, and
// anyone who could write to the state directory could have the App remove
// any runner. With create, a missing directory is made, readable by its
// owner alone.
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

// prepareDirs makes the state directory and the pools' JIT config
// directories, and checks the ones that already exist.
func prepareDirs(stateDir string, pools map[string]*config.Pool) error {
	return eachDir(stateDir, pools, func(dir string) error { return privateDir(dir, true) })
}

// CheckDirs checks the directories in cfg that already exist, as Run would.
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
