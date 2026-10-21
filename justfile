# runner-controller: every command this repo has. `just` with no arguments lists them.
#
# scripts/check-commits.py, scripts/check-claude-md.py and .githooks/ are
# copies of the maintainers' shared tooling and are never edited here.
#
# If the repo relies on a .env file, make the first line
# `set dotenv-load := true`.

default:
    @just --list

# --- The stack: Go (one module) --------------------------------------------
# The template has no Go snippet, so this is the equivalent of its Rust one,
# with the standard names.

[doc("Release build of the binary, into dist/")]
build:
    CGO_ENABLED=0 go build -trimpath -o dist/runner-controller ./cmd/runner-controller

# What the release workflow publishes: a static binary, and a SHA256SUMS an
# installer can pin it by.
[doc("Static linux/amd64 release binary and its SHA256SUMS, into dist/")]
release-build version:
    #!/usr/bin/env bash
    set -euo pipefail
    rm -rf dist && mkdir -p dist
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags "-s -w -X main.version={{ version }}" \
        -o dist/runner-controller-linux-amd64 ./cmd/runner-controller
    cd dist && sha256sum runner-controller-linux-amd64 > SHA256SUMS
    cat SHA256SUMS

[doc("Check a configuration file, its keys and its directories")]
check-config path:
    go run ./cmd/runner-controller -check -config {{ path }}

[doc("Compile every package")]
check:
    go build -o /dev/null ./...

[doc("go vet")]
lint:
    go vet ./...

[doc("Format everything, writing changes")]
format:
    gofmt -w .

[doc("Check formatting without writing (what CI runs)")]
format-check:
    #!/usr/bin/env bash
    set -euo pipefail
    unformatted="$(gofmt -l .)"
    if [ -n "$unformatted" ]; then
        echo "not gofmt-formatted:" >&2
        echo "$unformatted" >&2
        exit 1
    fi

[doc("Run the test suite, with the race detector")]
test:
    go test -race ./...

[doc("Remove build output")]
clean:
    rm -rf dist

# --- Shared checks ---------------------------------------------------------

# Add --no-scope (a repo without components yet) or --scopes a,b to match
# the repo's CLAUDE.md. The script takes them as flags so it never needs
# editing here. --require-issue: every branch links the issue it serves, in
# a `Refs:` or `Closes:` trailer, or says `Refs: none (<reason>)` when it
# has no feat or fix commit.
[doc("Conventional Commits on every commit since base, and an issue per branch")]
lint-commits base="origin/main":
    python3 scripts/check-commits.py --base {{ base }} --require-issue

# A credential that reaches git history stays there, so the whole history is
# scanned, not just the diff. CI installs gitleaks at a pinned version.
[doc("Fail on a credential anywhere in git history")]
scan-secrets:
    gitleaks git --no-banner --redact --exit-code 1 .

[doc("CLAUDE.md and SKILL.md length, TODO headings and @-imports")]
check-claude-md:
    python3 scripts/check-claude-md.py

# What CI runs on every push, and what the pre-push hook runs before one.
# The stack's format-check, lint, check and test go first: cheapest first,
# so a formatting slip fails in a second, not after the suite.
[doc("Every check CI runs; the pre-push hook runs it too")]
verify-all: format-check lint check test lint-commits scan-secrets check-claude-md

# --- Shipping --------------------------------------------------------------

# Once per clone. GitHub's free plan has no branch protection on private
# repos, so these hooks and pr-merge are what refuse a commit or push to
# main or staging. See .githooks/ for the escape hatches.
#
# It also keeps an SSH push's connection alive. Git connects to the remote
# before pre-push runs and sends the pack only once the hook returns, so the
# connection sits idle for as long as verify-all takes. Over SSH that can be
# dropped, and the push then dies with SIGPIPE (exit 141) after a gate that
# passed, with nothing on the remote. Set only for
# an SSH remote, and never over a core.sshCommand set at any level, which
# may name a particular ssh; GIT_SSH_COMMAND still overrides it.
[doc("Install the git hooks: no commits on main/staging, verify-all before a push")]
install-hooks:
    #!/usr/bin/env bash
    set -euo pipefail
    git config core.hooksPath .githooks
    echo "installed: pre-commit refuses commits on main and staging"
    echo "installed: pre-push refuses pushes to them, then runs just verify-all (SKIP_VERIFY=1 skips the checks)"
    keepalive="ssh -o ServerAliveInterval=30 -o ServerAliveCountMax=10"
    case "$(git remote get-url origin 2>/dev/null || true)" in
        git@*|ssh://*) ;;
        *) exit 0 ;;
    esac
    current="$(git config --get core.sshCommand || true)"
    if [ -z "$current" ]; then
        git config --local core.sshCommand "$keepalive"
        echo "installed: an SSH keepalive (core.sshCommand), so a long pre-push can't let the push's connection drop"
    elif [ "$current" != "$keepalive" ]; then
        echo "kept: core.sshCommand is already set ($current); add ServerAliveInterval to it if pushes die with exit 141"
    fi

# The shared script, from the installed workflow plugin rather than a copy:
# it runs only on a developer's machine, never in CI.
[doc("Merge a PR on green with a merge commit, then remove its worktree and branches")]
pr-merge +args:
    #!/usr/bin/env bash
    set -euo pipefail
    script="$HOME/.claude/plugins/marketplaces/leal-software/plugins/workflow/skills/ship-change/scripts/pr-merge.sh"
    [ -f "$script" ] || { echo "pr-merge: install the workflow plugin first: claude plugin marketplace add LealSoftware/claude-plugins && claude plugin install workflow@leal-software" >&2; exit 1; }
    exec bash "$script" {{ args }}
