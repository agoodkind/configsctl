#!/usr/bin/env python3
"""Detect input-default and presence checks by parsing the Jinja AST.

A line regex cannot tell what a construct reads. This module parses each Jinja
expression with jinja2 and resolves every default or presence construct to its
true operand root, following nested filter chains, so `(smtp_user | trim) |
length > 0` resolves to `smtp_user` and `cmd.rc | default(1)` resolves to `cmd`.

The caller classifies each root. A root that names a declared input variable is a
violation, since an input value must be deterministic. A root that names a
registered task result, a gathered fact, a set_fact value, or a loop value is a
runtime value, so a defensive read of it is allowed.

This module owns the expression layer only. It takes a Jinja source string and
returns the constructs it finds. File walking, the declared-input set, and the
runtime set live in the caller.
"""
from __future__ import annotations

from dataclasses import dataclass

from jinja2 import Environment, nodes

_ENV = Environment()

# Runtime roots whose presence or shape is genuinely external, so a default or
# presence check on them is allowed. Registers and set_fact and loop names are
# added per file by the caller; these are the fixed magic names.
FACT_ROOTS = frozenset(
    {
        "ansible_facts",
        "ansible_local",
        "hostvars",
        "groups",
        "group_names",
        "inventory_hostname",
        "inventory_hostname_short",
        "ansible_play_hosts",
        "ansible_play_hosts_all",
        "play_hosts",
        "ansible_play_batch",
        "ansible_check_mode",
        "ansible_run_tags",
        "ansible_skip_tags",
        "ansible_version",
        "ansible_date_time",
        "item",
        "ansible_loop",
        "ansible_loop_var",
        "omit",
    }
)
FACT_PREFIX = "ansible_"

# Filter and test names that infer a value or its presence.
_DEFAULT_FILTERS = frozenset({"default", "d"})
_LENGTH_FILTERS = frozenset({"length", "count"})
_PRESENCE_TESTS = frozenset({"defined", "undefined", "none"})
_MEMBERSHIP_CONTAINERS = frozenset({"groups", "hostvars", "vars"})


@dataclass(frozen=True)
class Construct:
    """One default or presence construct found in an expression. kind names the
    form, root is the resolved operand variable (or None when it has none, as with
    a lookup default), and lineno is the 1-based line within the parsed source."""

    kind: str
    root: str | None
    lineno: int


def root_name(node: nodes.Node) -> str | None:
    """Resolve a node to the root variable it reads, descending through attribute
    access, subscripts, filters, and calls, so the operand of a nested filter
    chain comes back as its base Name. Return None when the base is not a Name."""
    current = node
    while True:
        if isinstance(current, (nodes.Getattr, nodes.Getitem, nodes.Filter)):
            current = current.node
        elif isinstance(current, nodes.Call):
            current = current.node
        else:
            break
    if isinstance(current, nodes.Name):
        return current.name
    return None


def _names_in(node: nodes.Node) -> set[str]:
    """Return every variable name referenced anywhere under a node."""
    return {name.name for name in node.find_all(nodes.Name)}


def find_constructs(source: str) -> list[Construct]:
    """Parse a Jinja source string and return every default or presence construct.
    Raise nothing on a parse error; an unparsable expression yields no constructs,
    which the caller treats as nothing found rather than a pass."""
    try:
        ast = _ENV.parse(source)
    except Exception:
        return []

    found: list[Construct] = []

    for filt in ast.find_all(nodes.Filter):
        if filt.name in _DEFAULT_FILTERS:
            found.append(Construct("default", root_name(filt.node), filt.lineno))

    for test in ast.find_all(nodes.Test):
        if test.name in _PRESENCE_TESTS:
            found.append(Construct("presence", root_name(test.node), test.lineno))

    for call in ast.find_all(nodes.Call):
        target = call.node
        if isinstance(target, nodes.Getattr) and target.attr == "get":
            kind = "get-default" if len(call.args) >= 2 else "get"
            found.append(Construct(kind, root_name(target.node), call.lineno))
        if isinstance(target, nodes.Name) and target.name in {"lookup", "query", "q"}:
            if any(getattr(kw, "key", None) == "default" for kw in call.kwargs):
                found.append(Construct("lookup-default", None, call.lineno))

    for comp in ast.find_all(nodes.Compare):
        if isinstance(comp.expr, nodes.Filter) and comp.expr.name in _LENGTH_FILTERS:
            found.append(Construct("length", root_name(comp.expr.node), comp.lineno))
        for operand in comp.ops:
            if operand.op in {"in", "notin"} and isinstance(operand.expr, nodes.Name):
                if operand.expr.name in _MEMBERSHIP_CONTAINERS:
                    found.append(
                        Construct("membership", root_name(comp.expr), comp.lineno)
                    )

    for cond in ast.find_all(nodes.CondExpr):
        if isinstance(cond.test, nodes.Name) and cond.test.name in _names_in(cond.expr1):
            found.append(Construct("self-ternary", cond.test.name, cond.lineno))

    return found


def is_violation(construct: Construct, runtime_names: frozenset[str]) -> bool:
    """Decide whether a construct violates the rule. A lookup default always does.
    Any other construct violates unless its root is a runtime value, which is a
    register or set_fact or loop name (passed in) or a fixed fact name. An
    unresolved root flags, which is the safe direction for an enforcement gate."""
    if construct.kind == "lookup-default":
        return True
    root = construct.root
    if root is None:
        return True
    if root in runtime_names or root in FACT_ROOTS or root.startswith(FACT_PREFIX):
        return False
    return True


_SELFTEST_CASES = [
    # (source, runtime_names, expected_violation_kinds)
    ("{{ x | default('') }}", set(), {"default"}),
    ("{{ x | d('') }}", set(), {"default"}),
    ("{{ cmd.rc | default(1) }}", {"cmd"}, set()),
    ("{{ (smtp_user | trim) | length > 0 }}", set(), {"length"}),
    ("{{ guests | length }} guests", set(), set()),
    ("{{ x is defined }}", set(), {"presence"}),
    ("{{ x is undefined }}", set(), {"presence"}),
    ("{{ x is none }}", set(), {"presence"}),
    ("{{ ansible_default_ipv4 is defined }}", set(), set()),
    ("{{ d.get('k') }}", set(), {"get"}),
    ("{{ d.get('k', 0) }}", set(), {"get-default"}),
    ("{{ a + '\\n' if a else '' }}", set(), {"self-ternary"}),
    ("{{ 'true' if flag else 'false' }}", set(), set()),
    ("{{ vault_a if env == 'testbed' else vault_b }}", set(), set()),
    ("{{ g in groups }}", set(), {"membership"}),
    ("{{ inventory_hostname in groups['consul_servers'] }}", set(), set()),
    ("{{ lookup('env', 'X', default='y') }}", set(), {"lookup-default"}),
]


def _selftest() -> int:
    failures = 0
    for source, runtime, expected in _SELFTEST_CASES:
        constructs = find_constructs(source)
        violating = {c.kind for c in constructs if is_violation(c, frozenset(runtime))}
        status = "ok" if violating == expected else "FAIL"
        if violating != expected:
            failures += 1
        print(f"  [{status}] {source!r} -> {sorted(violating)} (want {sorted(expected)})")
    print(f"\n{len(_SELFTEST_CASES) - failures}/{len(_SELFTEST_CASES)} cases passed.")
    return 1 if failures else 0


if __name__ == "__main__":
    import sys

    if "--selftest" in sys.argv[1:]:
        sys.exit(_selftest())
    print("usage: lint_ansible_ast.py --selftest", file=sys.stderr)
    sys.exit(2)
