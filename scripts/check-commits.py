#!/usr/bin/env python3
"""Conventional Commits check over every commit on the branch.

    check-commits.py [--base origin/main] [--target main]
                     [--no-scope | --scopes a,b] [--require-issue]

Copied from the shared workflow plugin's justfile-conventions template.
Never edit it here: change it there and copy it back, so every repo checks
commits the same way. A repo's own rules come in as flags, which is why the
script never needs editing. `--version` says which version of the script a
copy is.

Every commit since the merge base with --base, merges excluded (GitHub's
"Merge pull request" isn't one we author), must be
`<type>(<scope>)!: <description>` with a known type, a lowercase first letter,
no trailing period, and a header of at most 72 characters. The scope is
optional and any lowercase word unless --no-scope (the repo has no
components yet) or --scopes (only these) says otherwise.

`git revert`'s own header, `Revert "<reverted header>"`, passes as it is: git
writes it, the version tooling reads it as a revert, and the header it quotes
was checked when that commit landed, or was a merge. A revert written by hand
is `revert: <description>` like any other type.

Two footer rules always apply, because release versions and issue state are
derived from commits. They read the footers, which are the message's last
paragraph, so prose in the body such as "Fixes: the race in ..." or
"Breaking changes: none" is left alone:

- A breaking-change footer is spelled `BREAKING-CHANGE:` (or `BREAKING
  CHANGE:`, which the spec also allows). `Breaking change:` or `BREAKING:`
  reads to the version tooling as nothing at all, so the release would come
  out a patch. In the trailer block it is `BREAKING-CHANGE:`: git reads no
  trailers from a paragraph holding `BREAKING CHANGE:`, since a trailer key
  has no space, so a misspelling is told to use the hyphen.
- `Closes:` (or Fixes/Resolves) only on a branch into main. An issue closes
  when its fix reaches main; a branch into staging says `Refs:` instead, and
  the issue stays open until the promotion.

  The branch the commits go into is, in order:
  - --target, when given;
  - the base's own name, when the base is a branch (`origin/main` is
    `main`);
  - in CI, the branch the run is for: `GITHUB_BASE_REF` on a pull request,
    `GITHUB_REF_NAME` on a push.
  A base that is a commit, such as a push compared against the commit before
  it, says nothing about where the commits go. Without --target or a CI
  variable the target is unknown, and the `Closes:` rule is skipped, with a
  line saying so, rather than refusing a trailer it can't judge.

With --require-issue, at least one commit on the branch carries a `Refs:` or
`Closes:` trailer naming an issue (#N, owner/repo#N or a URL). `Refs: none
(<reason>)` passes instead, but only on a branch with no feat or fix commit:
a branch that adds or fixes something links the issue it serves. A trailer
is what git reads as one: a line in the message's last paragraph. A
`Closes: #N` separated from `Co-Authored-By:` by a blank line is not, and
the failure says so.

Commits by dependabot[bot] are skipped. Its headers are generated, not
authored, and dependabot.yml's commit-message prefix already types them,
but a grouped update reads "build: Bump the ... group across 1 directory
with 2 updates": capitalised and over 72 characters, which would fail every
grouped Dependabot PR's CI. A branch of only Dependabot commits needs no
issue.

Stdlib only, so a repo needs no Node toolchain for it.
"""
import argparse
import os
import re
import subprocess
import sys

TYPES = "feat|fix|docs|style|refactor|perf|test|build|ci|chore|revert"
HEADER_RE = re.compile(
    rf"^(?P<type>{TYPES})(?:\((?P<scope>[a-z0-9_-]+)\))?(?P<bang>!)?: (?P<desc>\S.*)$"
)
GIT_REVERT_RE = re.compile(r'^Revert ".+"$')
MAX_HEADER = 72
GENERATED_BY = ("dependabot[bot]",)
__version__ = "0.22.0"

# A footer line that names an issue, whether or not git reads it as a trailer.
LINK_LINE_RE = re.compile(
    r"^(?P<key>refs|close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:\s*(?P<value>.+)$", re.I)
CLOSING_KEY_RE = re.compile(r"^(close[sd]?|fix(e[sd])?|resolve[sd]?)$", re.I)
ISSUE_RE = re.compile(r"^(#\d+|[\w.-]+/[\w.-]+#\d+|https?://\S+/(issues|pull)/\d+)$")
NONE_RE = re.compile(r"^none \(\s*\S.*\)$")
BREAKING_RE = re.compile(r"^(?P<token>breaking(?:[ _-]?changes?)?)\s*:", re.I)
BREAKING_TOKENS = ("BREAKING CHANGE", "BREAKING-CHANGE")
RECORD_FORMAT = "%h%x00%an%x00%s%x00%B%x00%(trailers:only,unfold)%x1e"


def commits(base):
    log = subprocess.run(
        ["git", "log", "--no-merges", f"--format={RECORD_FORMAT}", f"{base}..HEAD"],
        capture_output=True, text=True, check=True,
    ).stdout
    for record in log.split("\x1e"):
        record = record.strip("\n")
        if record:
            sha, author, header, message, trailers = record.split("\x00", 4)
            body = message.splitlines()[1:]
            yield sha, author, header, body, trailers.splitlines()


def commit_type(header):
    if GIT_REVERT_RE.match(header):
        return "revert"
    m = HEADER_RE.match(header)
    return m.group("type") if m else None


def header_problems(header, no_scope, allowed):
    if GIT_REVERT_RE.match(header):
        return []
    m = HEADER_RE.match(header)
    found = []
    if not m:
        found.append(f"not `<type>(<scope>): <description>` with a type in {TYPES}")
    else:
        desc, scope = m.group("desc"), m.group("scope")
        if desc[0].isupper():
            found.append("description starts with a capital letter")
        if desc.endswith("."):
            found.append("description ends with a period")
        if scope and no_scope:
            found.append("omit the scope: this repo has no components")
        elif scope and allowed and scope not in allowed:
            found.append(f"scope must be one of {', '.join(allowed)}")
    if len(header) > MAX_HEADER:
        found.append(f"header is {len(header)} characters, over {MAX_HEADER}")
    return found


def last_paragraph(body):
    paragraphs, current = [], []
    for line in body:
        if line.strip():
            current.append(line)
        elif current:
            paragraphs.append(current)
            current = []
    return current or (paragraphs[-1] if paragraphs else [])


def footer_problems(body, into_main):
    found = []
    last = last_paragraph(body)
    if len(last) > 1 and any(line.startswith("BREAKING CHANGE:") for line in last):
        found.append("git reads no trailers from a paragraph with `BREAKING CHANGE:` in it "
                     "(a trailer key has no space), so the rest of that block is lost: "
                     "write `BREAKING-CHANGE:`")
    for line in last:
        b = BREAKING_RE.match(line)
        if b and b.group("token") not in BREAKING_TOKENS:
            found.append(f"`{b.group('token')}:` isn't read as a breaking change, so the "
                         "release would be a patch: spell it `BREAKING-CHANGE:`")
        link = LINK_LINE_RE.match(line)
        if link and not into_main and CLOSING_KEY_RE.match(link.group("key")):
            found.append(f"`{line.strip()}` on a branch that doesn't merge into main: "
                         "use `Refs:`, and the issue closes when the promotion reaches main")
    return found


def issue_problems(authored):
    """Branch-level --require-issue findings, given (sha, type, body, trailers)."""
    found, linked, nones, loose = [], False, [], []
    for sha, _, body, trailers in authored:
        for trailer in trailers:
            m = LINK_LINE_RE.match(trailer)
            if not m:
                continue
            key, value = m.group("key"), m.group("value").strip()
            if NONE_RE.match(value) and key.lower() == "refs":
                nones.append(sha)
            elif all(ISSUE_RE.match(v) for v in re.split(r"[,\s]+", value) if v):
                linked = True
            else:
                found.append(f"{sha}: `{trailer}`: name #N, owner/repo#N or an issue URL, "
                             "or write `Refs: none (<reason>)`")
        loose += [(sha, line.strip()) for line in body
                  if LINK_LINE_RE.match(line) and line.strip() not in trailers]
    if linked or found:
        return found
    if nones:
        changes = [sha for sha, kind, _, _ in authored if kind in ("feat", "fix")]
        if changes:
            found.append(f"branch: `Refs: none` on a branch with feat or fix commits ({', '.join(changes)}): "
                         "link the issue the change serves")
        return found
    if loose:
        sha, line = loose[0]
        found.append(f"branch: no issue linked: {sha} has `{line}`, but git doesn't read it as a "
                     "trailer. Only the message's last paragraph is, so put it in the same "
                     "block as Co-Authored-By, with no blank line between")
    else:
        found.append("branch: no commit links an issue: add a `Refs: #N` trailer (`Closes: #N` into "
                     "main), or `Refs: none (<reason>)` on a branch with no feat or fix")
    return found


def target_branch(base, explicit):
    """The branch the commits merge into, or None when nothing says.

    The base's name is only evidence when the base is a branch: a commit
    such as `github.event.before` names no branch, and reading its last
    path component as one is what refused `Closes:` on pushes to main."""
    if explicit:
        return explicit
    ref = subprocess.run(["git", "rev-parse", "--verify", "--quiet", "--symbolic-full-name", base],
                         capture_output=True, text=True).stdout.strip()
    if ref.startswith("refs/heads/"):
        return ref[len("refs/heads/"):]
    if ref.startswith("refs/remotes/"):
        return ref[len("refs/remotes/"):].split("/", 1)[-1]
    return os.environ.get("GITHUB_BASE_REF") or os.environ.get("GITHUB_REF_NAME") or None


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--version", action="version", version=f"check-commits {__version__}")
    ap.add_argument("--base", default="origin/main")
    ap.add_argument("--target", default="",
                    help="the branch the commits merge into, when --base doesn't name it")
    group = ap.add_mutually_exclusive_group()
    group.add_argument("--no-scope", action="store_true", help="refuse any scope")
    group.add_argument("--scopes", default="", help="comma-separated scopes allowed")
    ap.add_argument("--require-issue", action="store_true",
                    help="at least one commit on the branch links an issue")
    args = ap.parse_args()
    allowed = [s for s in args.scopes.split(",") if s]
    target = target_branch(args.base, args.target)
    into_main = target in (None, "main")
    if target is None:
        print(f"lint-commits: {args.base!r} is a commit, not a branch, and neither --target nor CI "
              "says where these commits go, so `Closes:` wasn't checked against main")

    if subprocess.run(["git", "rev-parse", "--verify", "--quiet", args.base],
                      capture_output=True).returncode != 0:
        print(f"lint-commits: base {args.base!r} isn't available here, so nothing was checked")
        return 0

    problems = []
    skipped = 0
    authored = []
    for sha, author, header, body, trailers in commits(args.base):
        if author in GENERATED_BY:
            skipped += 1
            continue
        found = header_problems(header, args.no_scope, allowed) + footer_problems(body, into_main)
        problems += [f"{sha}: {header!r}: {p}" for p in found]
        authored.append((sha, commit_type(header), body, trailers))
    if args.require_issue and authored:
        problems += issue_problems(authored)

    for p in problems:
        print(p)
    note = f", skipped {skipped} by dependabot" if skipped else ""
    print(f"lint-commits {__version__}: {len(problems)} problem(s) since {args.base}{note}")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
