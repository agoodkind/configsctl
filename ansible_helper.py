#!/usr/bin/env python3
from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
import tempfile
from collections.abc import Mapping, Sequence
from pathlib import Path


def reexec_under_ansible_python() -> None:
    ansible_vault = shutil.which("ansible-vault")
    if ansible_vault is None:
        print("ansible-vault not found in PATH", file=sys.stderr)
        raise SystemExit(1)
    shebang = Path(ansible_vault).read_text(encoding="utf-8").splitlines()[0]
    if not shebang.startswith("#!"):
        print("ansible-vault has no shebang", file=sys.stderr)
        raise SystemExit(1)
    interpreter = shebang[2:].strip()
    if interpreter == sys.executable:
        print("ansible python is missing PyYAML", file=sys.stderr)
        raise SystemExit(1)
    os.execv(interpreter, [interpreter, __file__, *sys.argv[1:]])


try:
    import yaml
except ModuleNotFoundError:
    reexec_under_ansible_python()
    raise SystemExit(1)

CONFIGS_ROOT = Path(__file__).resolve().parent.parent
ANSIBLE_DIR = CONFIGS_ROOT / "ansible"
VAULT_PASS = Path.home() / ".config" / "ansible" / "vault.pass"
DEFAULT_VAULT_FILE = ANSIBLE_DIR / "inventory" / "group_vars" / "all" / "vault.yml"

YamlValue = (
    None
    | bool
    | int
    | float
    | str
    | list["YamlValue"]
    | dict[str, "YamlValue"]
)


def collect_key_paths(prefix: str, value: YamlValue, sink: list[str]) -> None:
    if isinstance(value, Mapping):
        for raw_key in sorted(value.keys(), key=str):
            key_name = str(raw_key)
            if prefix:
                key_path = f"{prefix}.{key_name}"
            else:
                key_path = key_name
            sink.append(key_path)
            collect_key_paths(key_path, value[raw_key], sink)
        return
    if isinstance(value, list):
        for item in value:
            collect_key_paths(prefix, item, sink)


def run_keys(vault_file: Path, vault_password_file: Path) -> int:
    command = [
        "ansible-vault",
        "view",
        "--vault-password-file",
        str(vault_password_file),
        str(vault_file),
    ]
    result = subprocess.run(command, check=False, capture_output=True, text=True)
    if result.returncode != 0:
        print("ansible-vault view failed", file=sys.stderr)
        return result.returncode
    loaded: YamlValue = yaml.safe_load(result.stdout)
    if loaded is None:
        return 0
    if not isinstance(loaded, Mapping):
        print("vault content is not a YAML mapping", file=sys.stderr)
        return 1
    paths: list[str] = []
    collect_key_paths("", loaded, paths)
    for path in paths:
        print(path)
    return 0


def run_secret(key: str, vault_file: Path, vault_password_file: Path) -> int:
    command = [
        "ansible-vault",
        "view",
        "--vault-password-file",
        str(vault_password_file),
        str(vault_file),
    ]
    result = subprocess.run(command, check=False, capture_output=True, text=True)
    if result.returncode != 0:
        sys.stderr.write(result.stderr)
        return result.returncode
    loaded: YamlValue = yaml.safe_load(result.stdout)
    if not isinstance(loaded, Mapping):
        print("vault content is not a YAML mapping", file=sys.stderr)
        return 1
    if key not in loaded:
        print(f"vault key not found: {key}", file=sys.stderr)
        return 1
    value = loaded[key]
    if not isinstance(value, str):
        print(f"vault key {key} is not a string", file=sys.stderr)
        return 1
    sys.stdout.write(value)
    return 0


def run_set_secrets(vault_file: Path, vault_password_file: Path) -> int:
    raw_input = sys.stdin.read()
    incoming = yaml.safe_load(raw_input) if raw_input.strip() else None
    if incoming is None:
        print("stdin was empty; nothing to merge", file=sys.stderr)
        return 1
    if not isinstance(incoming, Mapping):
        print("stdin must contain a YAML mapping of key -> value", file=sys.stderr)
        return 1

    view_command = [
        "ansible-vault",
        "view",
        "--vault-password-file",
        str(vault_password_file),
        str(vault_file),
    ]
    view_result = subprocess.run(
        view_command, check=False, capture_output=True, text=True
    )
    if view_result.returncode != 0:
        sys.stderr.write(view_result.stderr)
        return view_result.returncode
    existing: YamlValue = yaml.safe_load(view_result.stdout)
    if existing is None:
        existing = {}
    if not isinstance(existing, Mapping):
        print("vault content is not a YAML mapping", file=sys.stderr)
        return 1

    merged: dict[str, YamlValue] = {}
    for existing_key, existing_value in existing.items():
        merged[str(existing_key)] = existing_value
    for incoming_key, incoming_value in incoming.items():
        merged[str(incoming_key)] = incoming_value

    with tempfile.NamedTemporaryFile(
        "w", delete=False, suffix=".yml", dir=str(vault_file.parent)
    ) as plain:
        yaml.safe_dump(merged, plain, default_flow_style=False, sort_keys=False)
        plain_path = Path(plain.name)

    try:
        encrypt_command = [
            "ansible-vault",
            "encrypt",
            "--vault-password-file",
            str(vault_password_file),
            "--output",
            str(vault_file),
            str(plain_path),
        ]
        encrypt_result = subprocess.run(
            encrypt_command, check=False, capture_output=True, text=True
        )
        if encrypt_result.returncode != 0:
            sys.stderr.write(encrypt_result.stderr)
            return encrypt_result.returncode
    finally:
        plain_path.unlink(missing_ok=True)

    added_keys = sorted(set(incoming.keys()) - set(existing.keys()))
    updated_keys = sorted(set(incoming.keys()) & set(existing.keys()))
    if added_keys:
        print(f"added: {', '.join(added_keys)}")
    if updated_keys:
        print(f"updated: {', '.join(updated_keys)}")
    return 0


def resolve_playbook(playbook: str) -> Path:
    if playbook.endswith(".yml") or playbook.endswith(".yaml"):
        return Path(playbook)
    return Path("playbooks") / f"{playbook}.yml"


# One Ansible file pulls another in with these directives, with or without the
# ansible.builtin prefix. The deploy gate follows them to find the transitive set
# a deploy actually reads.
IMPORT_RE = re.compile(
    r"(?:ansible\.builtin\.)?"
    r"(?:import_playbook|include_playbook|import_tasks|include_tasks)\s*:\s*"
    r"([^\s{}'\"]+)"
)
# A template a playbook renders, referenced as `{{ repo_root }}/<service>/<file>.j2`.
# The token after the last Jinja brace is a path under the repo root.
J2_RE = re.compile(r"([^\s\"'{}]*\.j2)")


def deploy_scope_files(playbook: Path) -> list[str]:
    """Return the files a deploy reads: the playbook, every file it imports or
    includes (followed transitively, resolved next to the file that names it),
    and every `.j2` template those files render (resolved under the repo root).
    References that do not resolve to an existing scannable file are dropped."""
    worklist: list[Path] = [(ANSIBLE_DIR / playbook).resolve()]
    seen: set[Path] = set()
    found: set[Path] = set()
    while worklist:
        current = worklist.pop()
        if current in seen or not current.is_file():
            continue
        seen.add(current)
        if current.suffix in (".yml", ".yaml"):
            found.add(current)
        text = current.read_text(encoding="utf-8", errors="replace")
        for import_match in IMPORT_RE.finditer(text):
            target = (current.parent / import_match.group(1)).resolve()
            worklist.append(target)
        for template_match in J2_RE.finditer(text):
            token = template_match.group(1).lstrip("/")
            if not token:
                continue
            template = (CONFIGS_ROOT / token).resolve()
            if template.is_file() and template.suffix == ".j2":
                found.add(template)
    return sorted(str(path) for path in found)


def run_lint(paths: Sequence[str] | None = None) -> int:
    """Run the input-default linter. With paths, scan only those files (the files
    one deploy reaches); without, scan the whole tree. Nonzero means a banned
    `| default(...)` or `is defined` on an input variable is present."""
    linter = Path(__file__).resolve().parent / "lint_ansible_defaults.py"
    argv = [sys.executable, str(linter), *(paths or [])]
    return subprocess.run(argv, check=False).returncode


def run_deploy(
    playbook: str,
    limit: str | None,
    check: bool,
    diff: bool,
    extra_vars: Sequence[str],
    full_lint: bool,
) -> int:
    if full_lint:
        lint_rc = run_lint()
    else:
        lint_rc = run_lint(deploy_scope_files(resolve_playbook(playbook)))
    if lint_rc != 0:
        print(
            "deploy blocked: input-side default()/is defined found above; "
            "declare the value in group_vars and read it bare",
            file=sys.stderr,
        )
        return lint_rc
    command: list[str] = [
        "ansible-playbook",
        "--vault-password-file",
        str(VAULT_PASS),
        str(resolve_playbook(playbook)),
    ]
    if limit is not None:
        command.extend(["--limit", limit])
    if check:
        command.append("--check")
    if diff:
        command.append("--diff")
    for extra_var in extra_vars:
        command.extend(["--extra-vars", extra_var])
    result = subprocess.run(command, cwd=ANSIBLE_DIR, check=False)
    return result.returncode


def run_syntax_check(playbook: str) -> int:
    """Validate a playbook's structure with ansible-playbook --syntax-check.
    It parses the play and every file it imports or includes without connecting
    to any host, so it catches a broken include, a malformed task, or a bad
    template reference. Returns ansible's exit code."""
    command = [
        "ansible-playbook",
        "--syntax-check",
        "--vault-password-file",
        str(VAULT_PASS),
        str(resolve_playbook(playbook)),
    ]
    result = subprocess.run(command, cwd=ANSIBLE_DIR, check=False)
    return result.returncode


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Ansible helper for agent shells.")
    subparsers = parser.add_subparsers(dest="subcommand", required=True)

    keys_parser = subparsers.add_parser("keys", help="List vault key names.")
    keys_parser.add_argument("vault_file", nargs="?", default=str(DEFAULT_VAULT_FILE))
    keys_parser.add_argument(
        "--vault-password-file",
        dest="vault_password_file",
        default=str(VAULT_PASS),
    )

    deploy_parser = subparsers.add_parser("deploy", help="Run an ansible playbook.")
    deploy_parser.add_argument("playbook")
    deploy_parser.add_argument("--limit", default=None)
    deploy_parser.add_argument("--check", action="store_true")
    deploy_parser.add_argument("--diff", action="store_true")
    deploy_parser.add_argument(
        "--extra-var",
        dest="extra_var",
        action="append",
        default=[],
        metavar="KEY=VALUE",
        help="Pass one ansible extra var; repeatable. Example: --extra-var tack_image_tag=abc123",
    )
    deploy_parser.add_argument(
        "--full-lint",
        dest="full_lint",
        action="store_true",
        help="Scan the whole repo before deploying, instead of only the files this playbook reaches.",
    )

    syntax_parser = subparsers.add_parser(
        "syntax-check",
        help="Validate a playbook's structure (parse-only, no host connection).",
    )
    syntax_parser.add_argument("playbook")

    subparsers.add_parser(
        "lint", help="Run the input-default linter over the Ansible tree."
    )

    secret_parser = subparsers.add_parser(
        "secret", help="Print one vault secret value to stdout."
    )
    secret_parser.add_argument("key")
    secret_parser.add_argument(
        "--vault-file", dest="vault_file", default=str(DEFAULT_VAULT_FILE)
    )
    secret_parser.add_argument(
        "--vault-password-file",
        dest="vault_password_file",
        default=str(VAULT_PASS),
    )

    set_secrets_parser = subparsers.add_parser(
        "set-secrets",
        help="Merge a YAML mapping from stdin into the vault, preserving other keys.",
    )
    set_secrets_parser.add_argument(
        "--vault-file", dest="vault_file", default=str(DEFAULT_VAULT_FILE)
    )
    set_secrets_parser.add_argument(
        "--vault-password-file",
        dest="vault_password_file",
        default=str(VAULT_PASS),
    )

    return parser


def main(argv: Sequence[str]) -> int:
    args = build_parser().parse_args(list(argv))
    if args.subcommand == "keys":
        return run_keys(
            Path(args.vault_file).expanduser(),
            Path(args.vault_password_file).expanduser(),
        )
    if args.subcommand == "deploy":
        return run_deploy(
            args.playbook, args.limit, args.check, args.diff, args.extra_var, args.full_lint
        )
    if args.subcommand == "syntax-check":
        return run_syntax_check(args.playbook)
    if args.subcommand == "lint":
        return run_lint()
    if args.subcommand == "secret":
        return run_secret(
            args.key,
            Path(args.vault_file).expanduser(),
            Path(args.vault_password_file).expanduser(),
        )
    if args.subcommand == "set-secrets":
        return run_set_secrets(
            Path(args.vault_file).expanduser(),
            Path(args.vault_password_file).expanduser(),
        )
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
