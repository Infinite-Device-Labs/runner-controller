package controller

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
)

func TestPrivateDir(t *testing.T) {
	base := t.TempDir()

	made := filepath.Join(base, "made", "here")
	if err := privateDir(made, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(made); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("made %v, %v; want a 0700 directory", info, err)
	}
	if err := privateDir(filepath.Join(base, "absent"), false); err != nil {
		t.Errorf("a missing directory fails the check: %v", err)
	}

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(made, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{open: "can be written by users other than its owner", link: "symbolic link", file: "not a directory"} {
		for _, create := range []bool{true, false} {
			if err := privateDir(dir, create); err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("privateDir(%s, %v) = %v, want %q", dir, create, err, want)
			}
		}
	}
}

func TestRunRefusesAnOpenJITDirectory(t *testing.T) {
	h := newHarness(t, "10G", map[string]*config.Pool{"p": pool("ci-runner", "2G", 0, 0)}, set("a", "p"))
	dir := h.c.pools["p"].JITConfigDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o733); err != nil {
		t.Fatal(err)
	}
	err := h.c.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Errorf("Run = %v, want a refusal naming %s", err, dir)
	}
}
