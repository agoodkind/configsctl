#!/usr/bin/env python3
"""Ban input-side defaults and presence checks in Ansible.

Every input value must be declared explicitly in group_vars, inventory, or
OpenTofu. A playbook or template reads it directly and fails loudly when it is
missing. Inferring a value from whether it was set is banned in every form:
`| default(...)`, `is defined`, `.get(key, default)` (a default in disguise),
and any `| length` comparison (an "is this set" or "how big" check in disguise).

There is no automatic exception and no per-line escape hatch. The only defensible
reason to read a value defensively is a command result from an outside service
that is unset more often than not, and that judgment is the author's to make and
defend in review, not the check's to grant.

Usage:
  lint_ansible_defaults.py            scan the configs Ansible tree and the
                                      per-service template dirs
  lint_ansible_defaults.py FILE ...   scan only the given files (pre-commit mode)

Exit status is 1 when any input-side violation is found, 0 otherwise.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

# Scanned by default: every Jinja template (`*.j2`, always an Ansible template
# here) plus the Ansible tree (`ansible/`: playbooks, inventory, group_vars). No
# per-service list, so a new template or group_var is covered the moment it
# exists. docs, scripts, .agents, and mwan/go hold no Ansible input config.
ANSIBLE_TREE = "ansible"
SCAN_SUFFIXES = (".yml", ".yaml", ".j2")
EXCLUDE_DIR_PARTS = {".git", "node_modules", "go"}

DEFAULT_RE = re.compile(r"\|\s*default\s*\(")
ISDEF_RE = re.compile(r"\bis\s+(?:not\s+)?defined\b")
# Dict get with a default argument is `| default(...)` in disguise.
GET_DEFAULT_RE = re.compile(r"\.get\s*\(\s*[^,()]+,")
# Any length comparison infers whether a value is set or how big it is.
LENGTH_COMPARE_RE = re.compile(r"\|\s*length\s*\)?\s*(?:==|!=|<=|>=|<|>)")

# All four are banned outright. The only acceptable reason to read a value
# defensively is a command result from an outside service that is unset more
# often than not. That is the author's judgment to make and defend in review;
# the check has no automatic exception and no per-line escape hatch.
BANNED_PATTERNS = (DEFAULT_RE, ISDEF_RE, GET_DEFAULT_RE, LENGTH_COMPARE_RE)


def scan_file(path: Path) -> list[tuple[int, str]]:
    text = path.read_text(encoding="utf-8", errors="replace")
    violations: list[tuple[int, str]] = []
    for number, line in enumerate(text.splitlines(), start=1):
        if any(pattern.search(line) for pattern in BANNED_PATTERNS):
            violations.append((number, line.strip()))
    return violations


def candidate_files(args: list[str]) -> list[Path]:
    if args:
        paths = [Path(arg) for arg in args]
        return [
            p
            for p in paths
            if p.suffix in SCAN_SUFFIXES
            and not (EXCLUDE_DIR_PARTS & set(p.parts))
        ]
    files: set[Path] = set()
    for path in REPO_ROOT.rglob("*.j2"):
        if not (EXCLUDE_DIR_PARTS & set(path.parts)):
            files.add(path)
    for path in (REPO_ROOT / ANSIBLE_TREE).rglob("*"):
        if path.suffix in (".yml", ".yaml") and not (EXCLUDE_DIR_PARTS & set(path.parts)):
            files.add(path)
    return sorted(files)


def main(argv: list[str]) -> int:
    files = candidate_files(argv)
    total = 0
    for path in files:
        violations = scan_file(path)
        if not violations:
            continue
        rel = path.relative_to(REPO_ROOT) if path.is_absolute() else path
        for number, snippet in violations:
            print(f"{rel}:{number}: banned default or presence check: {snippet}")
            total += 1
    if total:
        print(f"\n{total} banned default / presence-check violation(s).")
        print("Declare every value in the service group_vars and read it bare.")
        print("Banned outright, no automatic exception and no escape hatch:")
        print("| default(...), is defined, .get(key, default), and any | length")
        print("comparison. The only defensible read is a command result from an")
        print("outside service that is unset more often than not.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
