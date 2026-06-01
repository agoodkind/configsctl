#!/usr/bin/env python3
"""Ban input-side `| default(...)` and `is defined` in Ansible.

Every input value must be declared explicitly in group_vars, inventory, or
OpenTofu. A playbook or template reads it directly and fails loudly when it is
missing. Inferring a value from whether it was set, with `| default(...)` or
`when: x is defined`, is banned.

Defaults and `is defined` are allowed on module or register OUTPUT (the shape of
a command result), because that guards a result, not a config input. The
allowlist covers the output-shape attributes and the registered variable names
collected per file. There is no per-line escape hatch; a genuine output-shape
read must take a form the allowlist recognizes.

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

# Attributes that mark a value as a command or register result, not a config
# input. Kept deliberately narrow so the check errs toward flagging.
RESULT_ATTRS = (
    "stdout",
    "stderr",
    "rc",
    "stat",
    "json",
    "results",
    "content",
    "changed",
    "skipped",
    "failed",
    "attempts",
    "msg",
    "cmd",
    "ansible_facts",
)

DEFAULT_RE = re.compile(r"\|\s*default\s*\(")
ISDEF_RE = re.compile(r"\bis\s+(?:not\s+)?defined\b")
REGISTER_RE = re.compile(r"^\s*register:\s*([A-Za-z_]\w*)")
BEFORE_DEFAULT_RE = re.compile(r"([A-Za-z_][\w.\[\]'\" )(]*?)\s*\|\s*default\s*\(")
BEFORE_ISDEF_RE = re.compile(r"([A-Za-z_][\w.\[\]'\" )(]*?)\s+is\s+(?:not\s+)?defined\b")
RESULT_ATTR_RE = re.compile(r"\.(?:%s)\b" % "|".join(RESULT_ATTRS))


def chain_is_output(chain: str, registers: set[str]) -> bool:
    """True when the expression piped into default()/is defined is a command or
    register result rather than a config input."""
    chain = chain.strip()
    root = re.match(r"[A-Za-z_]\w*", chain)
    if root and root.group(0) in registers:
        return True
    return bool(RESULT_ATTR_RE.search(chain))


def collect_registers(lines: list[str]) -> set[str]:
    found: set[str] = set()
    for line in lines:
        match = REGISTER_RE.match(line)
        if match:
            found.add(match.group(1))
    return found


def scan_file(path: Path) -> list[tuple[int, str]]:
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    registers = collect_registers(lines)
    violations: list[tuple[int, str]] = []
    for number, line in enumerate(lines, start=1):
        for filter_match in DEFAULT_RE.finditer(line):
            before = line[: filter_match.start()]
            chain_match = BEFORE_DEFAULT_RE.search(before + line[filter_match.start():])
            chain = chain_match.group(1) if chain_match else ""
            if not chain_is_output(chain, registers):
                violations.append((number, line.strip()))
                break
        else:
            for isdef_match in ISDEF_RE.finditer(line):
                chain_match = BEFORE_ISDEF_RE.search(line[: isdef_match.end()])
                chain = chain_match.group(1) if chain_match else ""
                if not chain_is_output(chain, registers):
                    violations.append((number, line.strip()))
                    break
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
            print(f"{rel}:{number}: input-side default()/is defined: {snippet}")
            total += 1
    if total:
        print(f"\n{total} input-side default()/is defined violation(s).")
        print("Declare the value in the service group_vars and read it bare.")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
