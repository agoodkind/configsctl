#!/usr/bin/env python3
"""Read, compare, and rewrite the input-default baseline file.

A baseline file records the findings that are already accepted. The linter fails
only on a finding whose key is absent from the baseline. Each finding reduces to
a stable key that survives a line-number change, so a finding that only moves
within a file keeps its identity.

The file format and the three update modes match the go-makefile baseline. The
header is `# <title>: generated_at=<date>`. Each row is
`<finding>\\t# <label>:first_added=<date> last_seen=<date>`.
"""
from __future__ import annotations

import re
from dataclasses import dataclass, field
from enum import Enum

# First :line: coordinate in a finding. Collapsing it to ":::" lets a finding
# match regardless of the line it sits on.
LINE_COORDINATE = re.compile(r":[0-9]+:")


class Mode(Enum):
    """A baseline update mode. SYNC records the current set and drops fixed rows.
    PRUNE_FIXED keeps only current findings already in the baseline. ACCEPT_NEW
    records the current set and keeps every old row."""

    SYNC = "sync"
    PRUNE_FIXED = "prune-fixed"
    ACCEPT_NEW = "accept-new"


# "remove-fixed" is an accepted spelling of PRUNE_FIXED.
_MODE_BY_NAME = {
    "sync": Mode.SYNC,
    "prune-fixed": Mode.PRUNE_FIXED,
    "remove-fixed": Mode.PRUNE_FIXED,
    "accept-new": Mode.ACCEPT_NEW,
}


def parse_mode(value: str) -> Mode:
    """Map a mode string to a Mode. Raise ValueError on an unknown mode."""
    mode = _MODE_BY_NAME.get(value)
    if mode is None:
        raise ValueError(f"unknown baseline update mode: {value}")
    return mode


def finding_key(finding: str) -> str:
    """Reduce a finding to a stable key: strip leading `../`, then replace the
    first `:line:` coordinate with `:::`."""
    stripped = finding
    while stripped.startswith("../"):
        stripped = stripped[len("../") :]
    match = LINE_COORDINATE.search(stripped)
    if match is None:
        return stripped
    return stripped[: match.start()] + ":::" + stripped[match.end() :]


def _is_skippable(line: str) -> bool:
    """Report whether an input line is ignored on read: blank, whitespace-only,
    or a comment beginning with `#`."""
    return line.strip() == "" or line.startswith("#")


def _first_added_from(metadata: str) -> str:
    """Extract the first_added value from a metadata suffix, or empty when absent."""
    for token in metadata.split():
        if token.startswith("first_added="):
            return token[len("first_added=") :]
    return ""


@dataclass(frozen=True)
class LoadedBaseline:
    """One parsed baseline file. order is the key insertion order. finding_by_key
    maps each key to its finding text without metadata. line_by_key maps each key
    to its full row. first_added_by_key maps each key to its recorded first_added
    date."""

    order: list[str] = field(default_factory=list)
    finding_by_key: dict[str, str] = field(default_factory=dict)
    line_by_key: dict[str, str] = field(default_factory=dict)
    first_added_by_key: dict[str, str] = field(default_factory=dict)

    def keys(self) -> set[str]:
        return set(self.finding_by_key)


def load_baseline(lines: list[str], label: str) -> LoadedBaseline:
    """Parse baseline body lines into a LoadedBaseline. Skip blank and comment
    lines. Split each row at the `\\t# <label>:` marker into finding and metadata."""
    marker = f"\t# {label}:"
    loaded = LoadedBaseline()
    for line in lines:
        if _is_skippable(line):
            continue
        finding = line
        metadata = ""
        marker_index = line.find(marker)
        if marker_index >= 0:
            finding = line[:marker_index]
            metadata = line[marker_index + len(marker) :]
        if finding == "":
            continue
        key = finding_key(finding)
        if key not in loaded.finding_by_key:
            loaded.order.append(key)
        loaded.finding_by_key[key] = finding
        loaded.line_by_key[key] = line
        loaded.first_added_by_key[key] = _first_added_from(metadata)
    return loaded


def evaluate(
    current_lines: list[str],
    baseline_keys: set[str],
) -> tuple[list[str], int]:
    """Compare current findings against the baseline keys. Return every finding
    whose key is absent from the baseline, in input order with no deduplication, so
    each banned line is listed on its own, and the gone count, the number of
    baseline keys absent from the current findings. A baseline key still suppresses
    every line that shares it, so accepting a finding accepts all its occurrences."""
    new_findings: list[str] = []
    current_keys: set[str] = set()
    for line in current_lines:
        key = finding_key(line)
        current_keys.add(key)
        if key in baseline_keys:
            continue
        new_findings.append(line)
    gone_count = sum(1 for key in baseline_keys if key not in current_keys)
    return new_findings, gone_count


def _current_index(current_lines: list[str]) -> tuple[list[str], dict[str, str]]:
    """Build the current key order and the key-to-line map, skipping blank and
    comment lines and keeping the first line seen for each key."""
    order: list[str] = []
    by_key: dict[str, str] = {}
    for line in current_lines:
        if _is_skippable(line):
            continue
        key = finding_key(line)
        if key not in by_key:
            order.append(key)
        by_key[key] = line
    return order, by_key


def rewrite_body(
    current_lines: list[str],
    old_lines: list[str],
    label: str,
    now: str,
    mode: Mode,
) -> list[str]:
    """Build the new baseline body for the given mode, preserving each row's
    original first_added date."""
    current_order, current_by_key = _current_index(current_lines)
    old = load_baseline(old_lines, label)

    def render_current(key: str) -> str:
        first_added = old.first_added_by_key.get(key, "") or now
        return (
            f"{current_by_key[key]}\t# {label}:"
            f"first_added={first_added} last_seen={now}"
        )

    body: list[str] = []
    if mode is Mode.SYNC:
        for key in current_order:
            body.append(render_current(key))
    elif mode is Mode.PRUNE_FIXED:
        for key in current_order:
            if key in old.finding_by_key:
                body.append(render_current(key))
    elif mode is Mode.ACCEPT_NEW:
        for key in current_order:
            body.append(render_current(key))
        for key in old.order:
            if key not in current_by_key:
                body.append(old.line_by_key[key])
    return body


def render_file(title: str, now: str, body: list[str]) -> str:
    """Assemble the baseline file: the generated_at header then the body rows."""
    lines = [f"# {title}: generated_at={now}"]
    lines.extend(body)
    return "\n".join(lines) + "\n"
