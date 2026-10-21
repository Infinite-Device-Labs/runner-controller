// Package config reads and checks the controller's configuration file.
//
// Everything site-specific lives in this file: the scale sets, their GitHub
// Apps, the pools of systemd units they start and the memory budget. Nothing
// about a particular deployment is compiled in.
package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultStateDir is where the controller keeps its record of the runners it
// started, unless the file names another directory.
const DefaultStateDir = "/var/lib/runner-controller"

// Config is the whole configuration file.
type Config struct {
	// MemoryBudget is the most memory the started units may declare between
	// them. Put the units in a slice whose MemoryMax is this value.
	MemoryBudget ByteSize `toml:"memory_budget"`
	// StateDir holds the record of started runners, so a restart can adopt
	// the units still running and clean up the rest.
	StateDir string `toml:"state_dir"`
	// Pools are keyed by name. A scale set names the pool it starts.
	Pools map[string]*Pool `toml:"pools"`
	// ScaleSets are the runner scale sets the controller holds a session for.
	ScaleSets []*ScaleSet `toml:"scale_sets"`
}

// Pool is a systemd template unit the controller starts one instance of per
// runner, and what each instance declares.
type Pool struct {
	// Name is the pool's key in the file.
	Name string `toml:"-"`
	// Unit is the template's name without "@.service": "ci-runner" starts
	// ci-runner@<slot>.service.
	Unit string `toml:"unit"`
	// JITConfigDir receives <slot>.env for each instance, the file only that
	// instance reads.
	JITConfigDir string `toml:"jit_config_dir"`
	// Memory is the limit the unit itself sets (its MemoryMax). It is what
	// one instance counts against the budget.
	Memory ByteSize `toml:"memory"`
	// MinRunners are kept started ahead of demand.
	MinRunners int `toml:"min_runners"`
	// MaxRunners caps the instances running at once. Zero means only the
	// budget limits them.
	MaxRunners int `toml:"max_runners"`
}

// ScaleSet is one runner scale set: where it is registered, the App that
// registers its runners, and the pool its runners run in.
type ScaleSet struct {
	// Name is the scale set's name on GitHub. Without Labels, it is also the
	// one label workflows target.
	Name string `toml:"name"`
	// URL is the organisation or repository the scale set is registered to,
	// such as https://github.com/example-org.
	URL string `toml:"url"`
	// Labels workflows target with runs-on. Defaults to Name.
	Labels []string `toml:"labels"`
	// RunnerGroup the scale set belongs to. Defaults to "default".
	RunnerGroup string `toml:"runner_group"`
	// Pool names the pool its runners are started in.
	Pool string `toml:"pool"`
	// GitHubApp registers the runners and holds the session.
	GitHubApp GitHubApp `toml:"github_app"`
}

// GitHubApp is the App a scale set authenticates as. Its key is read by the
// controller alone.
type GitHubApp struct {
	// ID is the App's ID or its client ID.
	ID AppID `toml:"id"`
	// InstallationID is the App's installation on the organisation or
	// repository.
	InstallationID int64 `toml:"installation_id"`
	// PrivateKeyPath is the App's PEM private key.
	PrivateKeyPath string `toml:"private_key_path"`
}

// Load reads the file at path and checks it.
func Load(path string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("reading %s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	if err := c.Check(); err != nil {
		return nil, fmt.Errorf("checking %s: %w", path, err)
	}
	return &c, nil
}

// unitName is what systemd allows in a unit's name before the "@".
var unitName = regexp.MustCompile(`^[A-Za-z0-9:_.-]+$`)

// safeName is what a scale set's, a runner group's or a pool's name may
// hold. actions/scaleset puts names into URLs unescaped, and a pool's name
// is part of each runner's.
var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// Check fills in defaults and reports every problem with the configuration.
func (c *Config) Check() error {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.MemoryBudget <= 0 {
		fail("memory_budget must be set")
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if !filepath.IsAbs(c.StateDir) {
		fail("state_dir %q must be an absolute path", c.StateDir)
	}

	if len(c.Pools) == 0 {
		fail("no pools")
	}
	units := map[string]string{}
	dirs := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(c.Pools)) {
		p := c.Pools[name]
		p.Name = name
		where := fmt.Sprintf("pool %q", name)
		if !safeName.MatchString(name) {
			fail("%s: a pool's name may only hold letters, digits, \".\", \"_\" and \"-\"", where)
		}
		switch {
		case p.Unit == "":
			fail("%s: unit must be set", where)
		case !unitName.MatchString(p.Unit):
			fail("%s: unit %q must be a template's name without \"@.service\", such as \"ci-runner\"", where, p.Unit)
		case units[p.Unit] != "":
			fail("%s: unit %q is also pool %q's", where, p.Unit, units[p.Unit])
		default:
			units[p.Unit] = name
		}
		p.JITConfigDir = filepath.Clean(p.JITConfigDir)
		switch {
		case !filepath.IsAbs(p.JITConfigDir):
			fail("%s: jit_config_dir %q must be an absolute path", where, p.JITConfigDir)
		case dirs[p.JITConfigDir] != "":
			fail("%s: jit_config_dir %q is also pool %q's", where, p.JITConfigDir, dirs[p.JITConfigDir])
		default:
			dirs[p.JITConfigDir] = name
		}
		if p.Memory <= 0 {
			fail("%s: memory must be set", where)
		} else if p.Memory > c.MemoryBudget {
			fail("%s: memory %s is more than the whole memory_budget %s", where, p.Memory, c.MemoryBudget)
		}
		if p.MinRunners < 0 || p.MaxRunners < 0 {
			fail("%s: min_runners and max_runners can't be negative", where)
		}
		if p.MaxRunners > 0 && p.MinRunners > p.MaxRunners {
			fail("%s: min_runners %d is more than max_runners %d", where, p.MinRunners, p.MaxRunners)
		}
	}

	if len(c.ScaleSets) == 0 {
		fail("no scale_sets")
	}
	names := map[string]bool{}
	users := map[string][]string{}
	for i, s := range c.ScaleSets {
		where := fmt.Sprintf("scale_sets[%d]", i)
		if s.Name == "" {
			fail("%s: name must be set", where)
		} else {
			where = fmt.Sprintf("scale set %q", s.Name)
			if !safeName.MatchString(s.Name) {
				fail("%s: a name may only hold letters, digits, \".\", \"_\" and \"-\"", where)
			}
			if names[s.Name] {
				fail("%s: the name is used twice", where)
			}
			names[s.Name] = true
		}
		if s.URL == "" {
			fail("%s: url must be set", where)
		}
		if s.RunnerGroup == "" {
			s.RunnerGroup = "default"
		} else if !safeName.MatchString(s.RunnerGroup) {
			fail("%s: runner_group %q may only hold letters, digits, \".\", \"_\" and \"-\"", where, s.RunnerGroup)
		}
		if c.Pools[s.Pool] == nil {
			fail("%s: pool %q isn't defined", where, s.Pool)
		} else {
			users[s.Pool] = append(users[s.Pool], s.Name)
		}
		app := s.GitHubApp
		if app.ID == "" || app.InstallationID == 0 || app.PrivateKeyPath == "" {
			fail("%s: github_app needs id, installation_id and private_key_path", where)
		}
	}
	for pool, sets := range users {
		if len(sets) > 1 && c.Pools[pool].MinRunners > 0 {
			fail("pool %q: min_runners needs a pool only one scale set uses, since a started runner belongs to one scale set; it is used by %s",
				pool, strings.Join(sets, ", "))
		}
	}

	return errors.Join(errs...)
}

// PrivateKey reads the App's key, and checks that it is an RSA key in PEM,
// as GitHub issues them. The key itself never appears in an error.
func (a GitHubApp) PrivateKey() (string, error) {
	b, err := os.ReadFile(a.PrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("reading the GitHub App's private key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", fmt.Errorf("%s holds no PEM-encoded key", a.PrivateKeyPath)
	}
	if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
		k, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if _, ok := k.(*rsa.PrivateKey); err8 != nil || !ok {
			return "", fmt.Errorf("%s isn't an RSA private key", a.PrivateKeyPath)
		}
	}
	return string(b), nil
}

// ByteSize is an amount of memory, written as systemd writes MemoryMax: a
// number of bytes with an optional K, M, G or T suffix, in powers of 1024.
type ByteSize int64

var byteSuffixes = map[byte]int64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}

// UnmarshalTOML accepts "4G" or a plain integer of bytes.
func (b *ByteSize) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		*b = ByteSize(v)
		return nil
	case string:
		n, err := ParseByteSize(v)
		*b = n
		return err
	default:
		return fmt.Errorf("a memory size must be a string such as \"4G\" or a number of bytes, not %T", v)
	}
}

// ParseByteSize parses "512M", "4G" or "1073741824".
func ParseByteSize(s string) (ByteSize, error) {
	t := strings.TrimSpace(s)
	mult := int64(1)
	if t != "" {
		if m, ok := byteSuffixes[strings.ToUpper(t[len(t)-1:])[0]]; ok {
			mult = m
			t = t[:len(t)-1]
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("%q isn't a memory size such as \"4G\"", s)
	}
	return ByteSize(n * mult), nil
}

func (b ByteSize) String() string {
	for _, s := range []byte{'T', 'G', 'M', 'K'} {
		m := byteSuffixes[s]
		if b != 0 && int64(b)%m == 0 {
			return fmt.Sprintf("%d%c", int64(b)/m, s)
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// AppID is an App's numeric ID or its client ID, so either can be written
// without quotes or with them.
type AppID string

// UnmarshalTOML accepts 123456 or "Iv1.abc123".
func (a *AppID) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		*a = AppID(strconv.FormatInt(v, 10))
	case string:
		*a = AppID(v)
	default:
		return fmt.Errorf("an App ID must be a number or a string, not %T", v)
	}
	return nil
}
