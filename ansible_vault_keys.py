#!/usr/bin/env python3
from __future__ import annotations

import argparse
import importlib
import os
import shutil
import subprocess
import sys
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import Protocol, TypeAlias, cast

DEFAULT_VAULT_FILE = Path("ansible/inventory/group_vars/all/vault.yml")
ERROR_MESSAGE = "failed to read vault keys; no decrypted content was printed"

YamlScalar: TypeAlias = None | bool | int | float | str
YamlValue: TypeAlias = YamlScalar | list["YamlValue"] | dict[str, "YamlValue"]
YamlKeyedValue: TypeAlias = (
    YamlScalar
    | list["YamlKeyedValue"]
    | dict[YamlScalar, "YamlKeyedValue"]
)


class YamlModule(Protocol):
    YAMLError: type[Exception]

    def safe_load(self, stream: str, /) -> YamlKeyedValue: ...


@dataclass(frozen=True)
class ParsedArgs:
    vault_file: Path
    vault_password_file: Path | None


def reexec_with_ansible_python() -> None:
    ansible_vault_path = shutil.which("ansible-vault")
    if ansible_vault_path is None:
        print(ERROR_MESSAGE, file=sys.stderr)
        raise SystemExit(1)

    try:
        first_line = Path(ansible_vault_path).read_text(
            encoding="utf-8"
        ).splitlines()[0]
    except (IndexError, OSError):
        print(ERROR_MESSAGE, file=sys.stderr)
        raise SystemExit(1)

    if not first_line.startswith("#!"):
        print(ERROR_MESSAGE, file=sys.stderr)
        raise SystemExit(1)

    interpreter_path = Path(first_line[2:].strip())
    current_interpreter = Path(sys.executable)
    if interpreter_path == current_interpreter:
        print(ERROR_MESSAGE, file=sys.stderr)
        raise SystemExit(1)

    try:
        os.execv(
            str(interpreter_path),
            [
                str(interpreter_path),
                str(Path(__file__).resolve()),
                *sys.argv[1:],
            ],
        )
    except OSError:
        print(ERROR_MESSAGE, file=sys.stderr)
        raise SystemExit(1)


def load_yaml_module() -> YamlModule:
    try:
        return cast(YamlModule, importlib.import_module("yaml"))
    except ModuleNotFoundError:
        reexec_with_ansible_python()
        raise RuntimeError("yaml module is unavailable")


YAML = load_yaml_module()
YAML_ERROR = YAML.YAMLError


def parse_args(argv: Sequence[str]) -> ParsedArgs:
    parser = argparse.ArgumentParser(
        description="List YAML key paths from an Ansible Vault file."
    )
    parser.add_argument(
        "vault_file",
        nargs="?",
        default=str(DEFAULT_VAULT_FILE),
        help=(
            "Path to the vault file. Defaults to "
            f"{DEFAULT_VAULT_FILE.as_posix()}."
        ),
    )
    parser.add_argument(
        "--vault-password-file",
        dest="vault_password_file",
        help="Pass through to ansible-vault view --vault-password-file.",
    )

    namespace = parser.parse_args(list(argv))

    vault_file = Path(namespace.vault_file).expanduser()
    vault_password_file: Path | None
    if namespace.vault_password_file is None:
        vault_password_file = None
    else:
        vault_password_file = Path(namespace.vault_password_file).expanduser()

    return ParsedArgs(
        vault_file=vault_file,
        vault_password_file=vault_password_file,
    )


def run_ansible_vault(args: ParsedArgs) -> str:
    command = ["ansible-vault", "view"]
    if args.vault_password_file is not None:
        command.extend(
            [
                "--vault-password-file",
                str(args.vault_password_file),
            ]
        )
    command.append(str(args.vault_file))

    try:
        result = subprocess.run(
            command,
            check=False,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError as error:
        raise RuntimeError("ansible-vault command was not found") from error

    if result.returncode != 0:
        raise RuntimeError("ansible-vault command failed")

    return result.stdout


def normalize_yaml_value(value: YamlKeyedValue) -> YamlValue:
    if isinstance(value, dict):
        normalized_mapping: dict[str, YamlValue] = {}
        for raw_key, raw_value in value.items():
            key_name = format_key(raw_key)
            normalized_mapping[key_name] = normalize_yaml_value(raw_value)
        return normalized_mapping

    if isinstance(value, list):
        normalized_items: list[YamlValue] = []
        for item in value:
            normalized_items.append(normalize_yaml_value(item))
        return normalized_items

    return value


def format_key(key: YamlScalar) -> str:
    if key is None:
        return "null"

    return str(key)


def print_key_paths(prefix: str, value: YamlValue) -> None:
    if isinstance(value, list):
        for item in value:
            print_key_paths(prefix, item)
        return

    if not isinstance(value, Mapping):
        return

    key_names = sorted(value.keys())
    for key_name in key_names:
        key_path: str
        if prefix:
            key_path = f"{prefix}.{key_name}"
        else:
            key_path = key_name
        print(key_path)
        print_key_paths(key_path, value[key_name])


def load_yaml_mapping(yaml_text: str) -> dict[str, YamlValue]:
    loaded_value = YAML.safe_load(yaml_text)
    normalized_value = normalize_yaml_value(loaded_value)

    if normalized_value is None:
        return {}

    if not isinstance(normalized_value, dict):
        raise ValueError("vault content must be a YAML mapping")

    return normalized_value


def main(argv: Sequence[str]) -> int:
    args = parse_args(argv)

    try:
        yaml_text = run_ansible_vault(args)
        yaml_mapping = load_yaml_mapping(yaml_text)
    except (RuntimeError, ValueError, YAML_ERROR):
        print(ERROR_MESSAGE, file=sys.stderr)
        return 1

    print_key_paths("", yaml_mapping)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
