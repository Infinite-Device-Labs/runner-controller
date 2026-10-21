package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheExampleLoads(t *testing.T) {
	c, err := Load("../../examples/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	if c.MemoryBudget != 48<<30 {
		t.Errorf("memory_budget %d", c.MemoryBudget)
	}
	if c.StateDir != DefaultStateDir {
		t.Errorf("state_dir %q", c.StateDir)
	}
	small := c.Pools["small"]
	if small.Name != "small" || small.Unit != "ci-runner" || small.Memory != 4<<30 || small.MinRunners != 2 {
		t.Errorf("pool small: %+v", small)
	}
	s := c.ScaleSets[1]
	if s.RunnerGroup != "default" || s.GitHubApp.ID != "Iv23liEXAMPLE" {
		t.Errorf("scale set: %+v", s)
	}
	if c.ScaleSets[0].GitHubApp.ID != "123456" {
		t.Errorf("a numeric App ID reads as %q", c.ScaleSets[0].GitHubApp.ID)
	}
}

const valid = `
memory_budget = "8G"
[pools.p]
unit = "ci-runner"
jit_config_dir = "/run/rc/p"
memory = "2G"
[[scale_sets]]
name = "a"
url = "https://github.com/example-org"
pool = "p"
github_app = { id = 1, installation_id = 2, private_key_path = "/k.pem" }
`

func load(t *testing.T, text string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	return err
}

func TestValid(t *testing.T) {
	if err := load(t, valid); err != nil {
		t.Fatal(err)
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		name, replace, with, want string
	}{
		{"no budget", `memory_budget = "8G"`, ``, "memory_budget must be set"},
		{"bad size", `"8G"`, `"8Q"`, `"8Q" isn't a memory size`},
		{"unknown key", `memory = "2G"`, "memory = \"2G\"\nmemroy = 1", "unknown keys: pools.p.memroy"},
		{"pool bigger than budget", `memory = "2G"`, `memory = "9G"`, "more than the whole memory_budget"},
		{"unit with suffix", `unit = "ci-runner"`, `unit = "ci-runner@.service"`, "without \"@.service\""},
		{"relative dir", `"/run/rc/p"`, `"rc/p"`, "must be an absolute path"},
		{"undefined pool", `pool = "p"`, `pool = "q"`, `pool "q" isn't defined`},
		{"min over max", `memory = "2G"`, "memory = \"2G\"\nmin_runners = 3\nmax_runners = 2", "min_runners 3 is more than max_runners 2"},
		{"no app", `github_app = { id = 1, installation_id = 2, private_key_path = "/k.pem" }`, ``, "github_app needs"},
		{"relative state dir", `memory_budget = "8G"`, "memory_budget = \"8G\"\nstate_dir = \"state\"", "state_dir"},
		{"name unsafe in a URL", `name = "a"`, `name = "a&name=b"`, "a name may only hold"},
		{"pool name unsafe", "[pools.p]", "[pools.\"p q\"]", "a pool's name may only hold"},
		{"runner group unsafe", `pool = "p"`, "pool = \"p\"\nrunner_group = \"x y\"", "runner_group \"x y\" may only hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(valid, tc.replace) {
				t.Fatalf("%q isn't in the valid config", tc.replace)
			}
			err := load(t, strings.Replace(valid, tc.replace, tc.with, 1))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestMinRunnersNeedAPoolOfItsOwn(t *testing.T) {
	text := strings.Replace(valid, `memory = "2G"`, "memory = \"2G\"\nmin_runners = 1", 1) + `
[[scale_sets]]
name = "b"
url = "https://github.com/example-org"
pool = "p"
github_app = { id = 1, installation_id = 2, private_key_path = "/k.pem" }
`
	err := load(t, text)
	if err == nil || !strings.Contains(err.Error(), "min_runners needs a pool only one scale set uses") {
		t.Errorf("got %v", err)
	}
	if err := load(t, strings.Replace(text, "min_runners = 1", "", 1)); err != nil {
		t.Errorf("two scale sets sharing a pool without min_runners: %v", err)
	}
}

func TestDuplicates(t *testing.T) {
	text := valid + `
[pools.q]
unit = "ci-runner"
jit_config_dir = "/run/rc/p"
memory = "1G"
[[scale_sets]]
name = "a"
url = "https://github.com/example-org"
pool = "q"
github_app = { id = 1, installation_id = 2, private_key_path = "/k.pem" }
`
	err := load(t, text)
	for _, want := range []string{`unit "ci-runner" is also pool "p"'s`, `jit_config_dir "/run/rc/p" is also pool "p"'s`, "the name is used twice"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("got %v, want it to contain %q", err, want)
		}
	}
}

func TestByteSize(t *testing.T) {
	for in, want := range map[string]ByteSize{"0": 0, "512": 512, "4K": 4 << 10, "512M": 512 << 20, "4G": 4 << 30, "2t": 2 << 40, " 1G ": 1 << 30} {
		got, err := ParseByteSize(in)
		if err != nil || got != want {
			t.Errorf("ParseByteSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "G", "-1G", "1.5G", "4GB", "99999999999T"} {
		if _, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) took it", in)
		}
	}
	for b, want := range map[ByteSize]string{4 << 30: "4G", 1536 << 20: "1536M", 1000: "1000", 0: "0"} {
		if got := b.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(b), got, want)
		}
	}
}

func TestPrivateKey(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) GitHubApp {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return GitHubApp{PrivateKeyPath: path}
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(rsaKey)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ec8, _ := x509.MarshalPKCS8PrivateKey(ecKey)

	for name, b := range map[string][]byte{
		"pkcs1.pem": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}),
		"pkcs8.pem": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}),
	} {
		if _, err := write(name, b).PrivateKey(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, b := range map[string][]byte{
		"ec.pem":      pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ec8}),
		"garbage.pem": []byte("SECRET-NOT-PEM"),
		"broken.pem":  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("SECRET-BYTES")}),
	} {
		_, err := write(name, b).PrivateKey()
		if err == nil {
			t.Errorf("%s was accepted", name)
		} else if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("%s: the error quotes the key: %v", name, err)
		}
	}
	if _, err := (GitHubApp{PrivateKeyPath: filepath.Join(dir, "absent.pem")}).PrivateKey(); err == nil {
		t.Error("a missing key was accepted")
	}
}
