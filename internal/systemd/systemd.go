// Package systemd starts, stops and watches runner units through systemctl.
package systemd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// startTimeout bounds a start, which may include an image pull;
	// TimeoutStartSec usually ends it first.
	startTimeout = 15 * time.Minute
	// queryTimeout bounds every other call, so a wedged systemd can't hang
	// the controller or its shutdown.
	queryTimeout = time.Minute
)

// Manager is what the controller asks of systemd.
type Manager interface {
	// Start starts a unit and waits for the start job to finish.
	Start(ctx context.Context, unit string) error
	// Stop queues a stop without waiting for it.
	Stop(ctx context.Context, unit string) error
	// ActiveStates returns each unit's ActiveState; an unloaded unit is
	// "inactive".
	ActiveStates(ctx context.Context, units []string) (map[string]string, error)
	// List returns the loaded units matching a glob such as
	// "ci-runner@*.service", with their ActiveState.
	List(ctx context.Context, pattern string) (map[string]string, error)
	// ResetFailed clears a failed unit's state.
	ResetFailed(ctx context.Context, unit string) error
}

// Exited reports whether state is one a stopped unit is left in.
func Exited(state string) bool {
	return state == "inactive" || state == "failed"
}

// Systemctl is a Manager for the system instance, via systemctl.
type Systemctl struct{}

func (Systemctl) Start(ctx context.Context, unit string) error {
	_, err := systemctl(ctx, startTimeout, "start", "--", unit)
	return err
}

func (Systemctl) Stop(ctx context.Context, unit string) error {
	_, err := systemctl(ctx, queryTimeout, "stop", "--no-block", "--", unit)
	return err
}

func (Systemctl) ActiveStates(ctx context.Context, units []string) (map[string]string, error) {
	if len(units) == 0 {
		return map[string]string{}, nil
	}
	out, err := systemctl(ctx, queryTimeout, append([]string{"show", "--property=Id,ActiveState", "--"}, units...)...)
	if err != nil {
		return nil, err
	}
	return parseShow(out)
}

func (Systemctl) List(ctx context.Context, pattern string) (map[string]string, error) {
	out, err := systemctl(ctx, queryTimeout, "list-units", "--all", "--plain", "--no-legend", "--full", "--", pattern)
	if err != nil {
		return nil, err
	}
	return parseList(out), nil
}

func (Systemctl) ResetFailed(ctx context.Context, unit string) error {
	_, err := systemctl(ctx, queryTimeout, "reset-failed", "--", unit)
	return err
}

func systemctl(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--no-pager", "--no-ask-password"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// parseShow parses `systemctl show --property=Id,ActiveState` for several
// units: blank-line-separated blocks.
func parseShow(out []byte) (map[string]string, error) {
	states := map[string]string{}
	var id, state string
	flush := func() {
		if id != "" {
			states[id] = state
		}
		id, state = "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			flush()
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "Id":
			id = v
		case "ActiveState":
			state = v
		}
	}
	flush()
	return states, sc.Err()
}

// parseList parses `systemctl list-units --plain --no-legend`: UNIT LOAD
// ACTIVE SUB DESCRIPTION per line.
func parseList(out []byte) map[string]string {
	units := map[string]string{}
	for line := range strings.Lines(string(out)) {
		f := strings.Fields(line)
		if len(f) >= 3 {
			units[f[0]] = f[2]
		}
	}
	return units
}
