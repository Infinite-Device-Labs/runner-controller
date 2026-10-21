// Package systemd starts and watches the units the controller runs runners
// in, through systemctl.
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
	// startTimeout bounds a start, which can include pulling an image.
	// systemd's own TimeoutStartSec usually ends it first.
	startTimeout = 15 * time.Minute
	// queryTimeout bounds every other call, so a wedged systemd can't hold
	// the controller, or its shutdown, for ever.
	queryTimeout = time.Minute
)

// Manager is what the controller asks of systemd.
type Manager interface {
	// Start starts a unit and returns once systemd has, or once the start
	// has failed.
	Start(ctx context.Context, unit string) error
	// Stop asks systemd to stop a unit and returns without waiting for it
	// to have stopped.
	Stop(ctx context.Context, unit string) error
	// ActiveStates returns each unit's ActiveState: "active", "inactive",
	// "failed", "activating" and so on. A unit systemd hasn't loaded is
	// "inactive".
	ActiveStates(ctx context.Context, units []string) (map[string]string, error)
	// List returns the loaded units that match a glob such as
	// "ci-runner@*.service", with their ActiveState.
	List(ctx context.Context, pattern string) (map[string]string, error)
	// ResetFailed clears a failed unit's state.
	ResetFailed(ctx context.Context, unit string) error
}

// Exited reports whether an ActiveState is one a unit is left in once it
// has stopped.
func Exited(state string) bool {
	return state == "inactive" || state == "failed"
}

// Systemctl is a Manager that runs systemctl against the system manager.
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

// parseShow reads `systemctl show --property=Id,ActiveState` for several
// units: one block of properties per unit, separated by blank lines.
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

// parseList reads `systemctl list-units --plain --no-legend`: a unit per
// line, as UNIT LOAD ACTIVE SUB DESCRIPTION.
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
