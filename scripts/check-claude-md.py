#!/usr/bin/env python3
"""Fail on a CLAUDE.md or SKILL.md that has drifted into a dumping ground.

    check-claude-md.py [--max-lines 200]

Copied from the shared workflow plugin's justfile-conventions template.
Never edit it here: change it there and copy it back. A repo with a reason
for a bigger root file passes --max-lines. `--version` says which version
of the script a copy is.

For every tracked or new CLAUDE.md and SKILL.md:

- a CLAUDE.md over --max-lines (default 200, Anthropic's guidance) and a
  SKILL.md over 500. Every CLAUDE.md loads into a session that touches its
  directory, and length costs adherence, not just context. The fix is
  almost never trimming prose: move what one directory needs into that
  directory's CLAUDE.md, and a procedure into a skill.
- a TODO heading. Deferred work is a GitHub issue, which gets watched and
  closed; a TODO line in a file read every session goes stale silently.
- an @-import in a CLAUDE.md. Imports load eagerly at session start, so
  importing sharded files back undoes the sharding while the file still
  looks short. Refer to other docs by plain path instead.
"""
import argparse
import pathlib
import re
import subprocess
import sys

SKILL_MAX = 500
__version__ = "0.26.0"
TODO_RE = re.compile(r"^#+\s*to-?do\b", re.I | re.M)
# A fence opens with three or more backticks or tildes, indented up to three
# spaces, and closes on a line of nothing but at least as many of the same.
FENCE_RE = re.compile(r"^ {0,3}(`{3,}|~{3,})(.*)$")
# `@` at line start or after whitespace, then something path-shaped. Not
# `pkg@1.2`, an email address or a DNS selector, all preceded by a non-space.
IMPORT_RE = re.compile(r"(?:^|\s)@[./~][^\s`]*|(?:^|\s)@[A-Za-z0-9_-]+/[^\s`]*")


def strip_fences(text):
    """Drop fenced code blocks, where `# todo` is a shell comment rather than
    a heading. An unclosed fence runs to the end, as Markdown renders it."""
    kept, fence = [], None
    for line in text.splitlines(keepends=True):
        m = FENCE_RE.match(line)
        if fence is None:
            if m:
                fence = m.group(1)
            else:
                kept.append(line)
        elif m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence) \
                and not m.group(2).strip():
            fence = None
    return "".join(kept)


def strip_code(text):
    """Drop fenced blocks and inline spans, where an @ is not an import."""
    return re.sub(r"`[^`\n]*`", "", strip_fences(text))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--version", action="version", version=f"check-claude-md {__version__}")
    ap.add_argument("--max-lines", type=int, default=200)
    args = ap.parse_args()

    # NUL-separated, so a path with a space or a non-ASCII name comes back
    # whole and unquoted.
    files = [f for f in subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard",
         "--", "*CLAUDE.md", "*SKILL.md"],
        capture_output=True, text=True, check=True,
    ).stdout.split("\0") if f]
    problems = []
    for name in files:
        path = pathlib.Path(name)
        if not path.is_file():
            continue
        text = path.read_text()
        lines = text.count("\n")
        is_skill = path.name == "SKILL.md"
        limit = SKILL_MAX if is_skill else args.max_lines
        if lines > limit:
            problems.append(f"{name}: {lines} lines, over its {limit}. Move what one "
                            "directory needs into that directory's CLAUDE.md, and procedures into a skill")
        if TODO_RE.search(strip_fences(text)):
            problems.append(f"{name}: has a TODO heading. Track deferred work as a GitHub issue")
        if not is_skill:
            for m in IMPORT_RE.finditer(strip_code(text)):
                problems.append(f"{name}: @-import {m.group().strip()!r} loads eagerly at "
                                "session start. Refer to the file by plain path")
    for p in problems:
        print(p, file=sys.stderr)
    print(f"check-claude-md {__version__}: {len(files)} file(s), {len(problems)} problem(s)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
