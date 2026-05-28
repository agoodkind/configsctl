#!/usr/bin/env python3
from __future__ import annotations

import argparse
import os
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


def run_deploy(
    playbook: str,
    limit: str | None,
    check: bool,
    diff: bool,
) -> int:
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
        return run_deploy(args.playbook, args.limit, args.check, args.diff)
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
