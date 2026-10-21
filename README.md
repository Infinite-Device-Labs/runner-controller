# runner-controller

Starts ephemeral GitHub Actions runners on demand, on one Linux host, for any
number of runner scale sets across organisations, within one memory budget.

> **Status: under development.** Nothing here runs yet. The design below is
> what is being built.

## Why

A self-hosted runner is registered to one repository, one organisation or one
enterprise. Running several fixed pools on one machine splits its memory
between them, so one pool queues while another sits idle. GitHub's own answer,
actions-runner-controller, needs Kubernetes.

runner-controller does the same job for a single host with systemd and Podman.
It uses GitHub's [actions/scaleset](https://github.com/actions/scaleset)
library: it holds one long-poll session per runner scale set, outbound only,
so the host can sit behind NAT with nothing exposed.

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
  the scale set whose oldest job has waited longest. A pool can set a
  `max_runners` cap, and keep `min_runners` started ahead of demand.
- **Keys stay on the host.** The GitHub App keys that register runners are
  read by the controller alone. A job only ever sees a configuration that is
  good for that one job.
- **It cleans up.** It removes the registrations and leftover units of runners
  that died mid-job, or that a reboot interrupted.

Put the units in a systemd slice whose `MemoryMax` equals the budget. Then a
miscount in the controller ends in a killed job, not a starved host.

## Working on it

`just` lists every command. `just verify-all` is what CI runs; run
`just install-hooks` once after cloning.

## Licence

MIT. See [LICENSE](LICENSE).
