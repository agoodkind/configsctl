#!/usr/bin/env python3
"""Token gate for the Ansible input-default linter.

The gate opens only when the operator supplies a confirm value and a token whose
slug equals the slug of the gate token command's stdout. Two environment-driven
checks build on the same primitives: a bypass that makes one lint run
non-blocking, and a baseline-write gate that authorizes a baseline refresh. When
the environment variables are absent, or the supplied token does not match, the
gate stays shut and the caller behaves as if the gate were not present.

The token command runs through `sh -c` and only after the confirm precondition
holds, so a routine run that sets no confirm value never invokes it.
"""
from __future__ import annotations

import os
import subprocess

# Default gate token command. It fetches today's Wikipedia featured-article
# canonical title and prints it. Slugification happens in slugify(), so this
# command emits the raw title and both sides run through the same slug. Override
# per call with BYPASS_TOKEN_CMD or BASELINE_TOKEN_CMD.
DEFAULT_GATE_TOKEN_CMD = (
    'curl -fsSL '
    '"https://en.wikipedia.org/api/rest_v1/feed/featured/'
    '$(date -u +%Y/%m/%d)" | jq -r ".tfa.titles.canonical"'
)

# Confirm values accepted as affirmative, matching the go-makefile gate.
AFFIRMATIVE_CONFIRM_VALUES = frozenset({"1", "y", "yes", "Y", "YES"})

# Bypass requires this exact confirm value, matching the go-makefile bypass arm.
BYPASS_CONFIRM_VALUE = "1"


def slugify(text: str) -> str:
    """Lowercase the text, keep `a`-`z`, `0`-`9`, `_`, and `-`, drop the rest."""
    characters: list[str] = []
    for char in text:
        if "A" <= char <= "Z":
            characters.append(char.lower())
        elif "a" <= char <= "z":
            characters.append(char)
        elif "0" <= char <= "9":
            characters.append(char)
        elif char in ("_", "-"):
            characters.append(char)
    return "".join(characters)


def confirm_accepted(value: str) -> bool:
    """Report whether the confirm value is one of the affirmative values."""
    return value in AFFIRMATIVE_CONFIRM_VALUES


def run_token_command(command: str) -> tuple[str, bool]:
    """Run the token command through `sh -c`. Return its stdout and whether it
    exited zero. A nonzero exit leaves the gate shut."""
    result = subprocess.run(
        ["sh", "-c", command],
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return "", False
    return result.stdout, True


def tokens_match(expected_raw: str, actual_raw: str) -> bool:
    """Report whether the two tokens are equal after slugification. An empty
    expected or actual slug never matches, so a failed token command or a missing
    operator token cannot open the gate."""
    expected = slugify(expected_raw)
    actual = slugify(actual_raw)
    if expected == "" or actual == "":
        return False
    return expected == actual


def bypass_passes() -> tuple[bool, str]:
    """Report whether one lint run should be non-blocking, and the matched token.

    Reads BYPASS_LINT, BYPASS_CONFIRM, and BYPASS_TOKEN_CMD. The bypass opens only
    when the BYPASS_LINT slug is non-empty, BYPASS_CONFIRM is exactly "1", and the
    BYPASS_LINT slug equals the slug of the token command's stdout. The token
    command runs only after the two environment preconditions hold.
    """
    bypass_value = slugify(os.environ.get("BYPASS_LINT", ""))
    if bypass_value == "":
        return False, ""
    if os.environ.get("BYPASS_CONFIRM", "") != BYPASS_CONFIRM_VALUE:
        return False, ""
    token_command = os.environ.get("BYPASS_TOKEN_CMD", "") or DEFAULT_GATE_TOKEN_CMD
    if token_command == "":
        return False, ""
    expected_raw, ok = run_token_command(token_command)
    if not ok:
        return False, ""
    expected = slugify(expected_raw)
    if expected == "" or bypass_value != expected:
        return False, ""
    return True, expected


def baseline_gate_passes() -> bool:
    """Report whether a baseline refresh is authorized.

    Reads BASELINE_CONFIRM, BASELINE_TOKEN, and BASELINE_TOKEN_CMD. The gate opens
    only when BASELINE_CONFIRM is affirmative and the slug of the token command's
    stdout equals the BASELINE_TOKEN slug. The token command runs only after the
    confirm check, so a run that sets no confirm value never invokes it.
    """
    if not confirm_accepted(os.environ.get("BASELINE_CONFIRM", "")):
        return False
    token_command = os.environ.get("BASELINE_TOKEN_CMD", "") or DEFAULT_GATE_TOKEN_CMD
    if token_command == "":
        return False
    expected_raw, ok = run_token_command(token_command)
    if not ok:
        return False
    return tokens_match(expected_raw, os.environ.get("BASELINE_TOKEN", ""))
