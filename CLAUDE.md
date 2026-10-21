# runner-controller — agent notes

A Go service that starts ephemeral GitHub Actions runners on demand from
runner scale sets, as systemd units, within one memory budget. `README.md` is
the public overview. Keep both in step.

## This repository is public

- **Nothing here points at private work.** No links to, or references to,
  private repositories, their issues, their decision records or their
  hosts, here, in `README.md`, in code comments or in commit messages.
  Decisions about this repository are recorded privately by its maintainers,
  not here. Issues and commits reference this repository's own issues.
- **Nothing site-specific.** No org names, App IDs, repository names, unit
  names or paths of any real deployment. Everything site-specific comes from
  the config file. Examples use placeholders (`example-org`, `ci-runner`).

## Design rules

- **The controller decides; systemd runs.** It starts and stops units and
  writes each job's JIT config file. It never starts a container, mounts
  anything, or passes a key into a job.
- **Outbound connections only.** The host may be behind NAT.
- **The memory budget is counted from declared limits, never from usage.**
  The slice's `MemoryMax` is the backstop, not the plan.
- **actions/scaleset is in public preview**, and its interfaces may change.
  Keep it behind one small internal interface, so a change touches one
  package.

Use `just` for every command in this repo, never the underlying tool. Run
`just` with no arguments to list the recipes; each has a one-line
description there. If a command has no recipe, add one.

**`just verify-all` is the gate**, and CI runs exactly it on every push.
Run it before calling a change done. **`just install-hooks` once per
clone**: the hooks refuse commits and pushes to `main` and run
`verify-all` before a push. Merge with **`just pr-merge <branch>`**, which
waits for green. `scripts/check-*.py` and `.githooks/` are copies of the
workflow plugin's template: never edit them here.

## Git

- Commits, pushes, PRs and merges each need an explicit ask. Being asked to
  change something is not permission to commit it.
- [Conventional Commits](https://www.conventionalcommits.org/):
  `<type>(<scope>): <description>`, imperative, lowercase, no trailing
  period. Every branch links its issue in a `Refs: #N` trailer, in the
  same last paragraph as `Co-Authored-By:`. `just lint-commits` checks
  both; the `conventional-commits` skill has the footer rules.
- New work starts in its own worktree on a `worktree-<topic>` branch, never
  on `main` in the primary clone. Merge commits only.

## Tracking deferred work

A GitHub issue, never a TODO here: this file is read every session, and a
TODO in it is a tracker nobody watches.
