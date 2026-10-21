# runner-controller

Starts ephemeral GitHub Actions runners on demand, on one Linux host, for any
number of runner scale sets across organisations, within one memory budget.

> **Status: first release, not yet proven in production.** It uses GitHub's
> [actions/scaleset](https://github.com/actions/scaleset), which is in public
> preview.

## Why

A self-hosted runner is registered to one repository, one organisation or one
enterprise. Running several fixed pools on one machine splits its memory
between them, so one pool queues while another sits idle. GitHub's own answer,
actions-runner-controller, needs Kubernetes.

runner-controller does the same job for a single host with systemd. It holds
one long-poll session per runner scale set, outbound only, so the host can sit
behind NAT with nothing exposed.

## How it works

- **It decides; systemd runs the jobs.** For each waiting job, the controller
  requests a just-in-time runner configuration, writes it to a file only that
  job's unit reads, and starts a systemd template unit you provide, such as
  `ci-runner@3.service`. The unit owns everything about the job's isolation:
  its image, network, firewall, user namespace, seccomp profile and limits.
  The controller never starts a container itself.
- **One memory budget.** Each pool declares the memory limit its unit sets.
  A runner starts only while the limits of those already running, plus the new
  one, fit the budget. When the budget is full, the next free memory goes to
  the scale set whose oldest job has waited longest. That scale set waits for
  enough memory rather than being passed over for a smaller one, so a pool of
  large runners can't be starved. A pool can set a `max_runners` cap, and keep
  `min_runners` started ahead of demand; jobs that are waiting come before
  those.
- **Idle memory goes back.** A runner whose job was cancelled before it took
  one sits idle. Once a scale set has more runners than its jobs and its
  `min_runners` need, and a runner has run for a minute, the controller
  removes that runner's registration and then stops its unit. GitHub refuses
  to remove a runner that has just taken a job, and that runner is left to
  run it.
- **Keys stay on the host.** The GitHub App keys that register runners are
  read by the controller alone. A job only ever sees a configuration that is
  good for that one job.
- **It cleans up.** When a unit exits, the controller removes its JIT config
  file and its runner's registration, and resets the unit if it failed. On
  startup it does the same for the runners a crash or a reboot interrupted,
  and adopts the units still running, so restarting the controller doesn't
  stop a job. A runner that exits without running a job, or fails to start,
  holds its scale set back for a while, doubling up to five minutes, rather
  than being replaced at once. Whether it ran one is GitHub's word: a job's
  started event can lag by a long poll, so a runner GitHub still counts as
  running a job isn't a failure. A runner that dies mid-job leaves a job GitHub
  counts as running for several minutes; the controller doesn't start a
  runner for that job, and removes the dead runner's registration as soon as
  GitHub lets go of it.

Put the units in a systemd slice whose `MemoryMax` equals the budget. Then a
miscount in the controller ends in a killed job, not a starved host.

## Installing

Each [release](https://github.com/Infinite-Device-Labs/runner-controller/releases)
has a static `linux/amd64` binary and a `SHA256SUMS` file:

```sh
version=v0.1.0
base=https://github.com/Infinite-Device-Labs/runner-controller/releases/download/$version
curl -fsSLO "$base/runner-controller-linux-amd64"
curl -fsSLO "$base/SHA256SUMS"
sha256sum -c SHA256SUMS
install -m 0755 runner-controller-linux-amd64 /usr/local/bin/runner-controller
```

Then, from [`examples/`](examples):

1. **A GitHub App** per organisation, installed on it, with the permission to
   manage self-hosted runners (organisation: *Self-hosted runners*, read and
   write; for a repository's scale set, the repository's *Administration*).
   Put its private key where only root can read it.
2. **A template unit per pool**, such as
   [`ci-runner@.service`](examples/systemd/ci-runner@.service). It reads
   `ACTIONS_RUNNER_INPUT_JITCONFIG` from `<jit_config_dir>/%i.env`, runs the
   runner, and exits when its one job is done. Its `MemoryMax` is the pool's
   `memory`. Starting it must return once the runner is running, so use
   `Type=simple`, `exec` or `notify`, not `oneshot`.
3. **The slice**, [`runner.slice`](examples/systemd/runner.slice), with
   `MemoryMax` set to the budget.
4. **The configuration**, `/etc/runner-controller/config.toml`, like
   [`config.toml`](examples/config.toml). `runner-controller -check` checks
   it, that each key is an RSA key in PEM, and the directories below.
5. **The controller's unit**,
   [`runner-controller.service`](examples/systemd/runner-controller.service).

## Configuration

```toml
memory_budget = "48G"               # what the units may declare in all

[pools.small]
unit = "ci-runner"                  # starts ci-runner@<slot>.service
jit_config_dir = "/run/runner-controller/small"   # writes <slot>.env, 0600
memory = "4G"                       # the unit's MemoryMax
min_runners = 0                     # kept started ahead of demand
max_runners = 0                     # 0: only the budget limits it

[[scale_sets]]
name = "example-linux"              # runs-on: example-linux
url = "https://github.com/example-org"   # or .../example-org/example-repo
labels = ["example-linux"]          # defaults to the name
runner_group = "default"
pool = "small"

[scale_sets.github_app]
id = 123456                         # the App's ID or its client ID
installation_id = 7890123
private_key_path = "/etc/runner-controller/example-org.pem"
```

The state directory and each `jit_config_dir` must be directories, not
symbolic links, owned by the controller's user and writable by no one else:
whoever could write to them could swap a job's configuration for their own.
The controller creates them, 0700, if they're missing, and refuses to start
otherwise. Names of scale sets, runner groups and pools hold only letters,
digits, `.`, `_` and `-`.

Several scale sets may share a pool, and then share its `max_runners`. A pool
with `min_runners` belongs to one scale set, since a started runner is
registered to one. Sizes are written as systemd writes `MemoryMax`, in powers
of 1024.

The controller creates each scale set if it doesn't exist, and updates its
labels if they differ. It never deletes one. Its runners are registered with
self-update off, so the unit's image decides the runner's version; keep it
current.

## Working on it

`just` lists every command. `just verify-all` is what CI runs; run
`just install-hooks` once after cloning.

- `internal/actions` is the only package that imports actions/scaleset, so a
  change in its preview API touches one package. It uses a patched copy of
  v0.4.0 in `internal/scaleset`, which adds the one call upstream lacks; its
  README says what changed.
- `internal/controller` decides: the budget, the order across scale sets, and
  cleanup. Its tests run against fakes of `actions.Client` and of systemd.
- `internal/systemd` runs `systemctl`.

## Licence

MIT. See [LICENSE](LICENSE).
