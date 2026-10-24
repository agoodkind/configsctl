#!/usr/bin/env bash

set -euo pipefail

: "${RUNNER_TEMP:?Set RUNNER_TEMP to the runner temporary directory}"
: "${GITHUB_PATH:?Set GITHUB_PATH to the runner path file}"
: "${RUNNER_OS:?Set RUNNER_OS to the runner operating system}"
: "${RUNNER_ARCH:?Set RUNNER_ARCH to the runner architecture}"

if [[ "$RUNNER_OS" != Linux ]]; then
  printf 'OpenTofu test setup requires a Linux runner.\n' >&2
  exit 1
fi
if [[ "$RUNNER_ARCH" != X64 ]]; then
  printf 'OpenTofu test setup requires an X64 runner.\n' >&2
  exit 1
fi

readonly OPENTOFU_VERSION=1.12.6
readonly OPENTOFU_ARCHIVE="$RUNNER_TEMP/opentofu.zip"
readonly OPENTOFU_CHECKSUM="$RUNNER_TEMP/opentofu.sha256"
readonly OPENTOFU_DIRECTORY="$RUNNER_TEMP/opentofu"

curl --fail --silent --show-error --location --output "$OPENTOFU_ARCHIVE" "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_linux_amd64.zip"
printf '5dc43da4f750f33873dc25e94587128709e819e544b7be9016b255316153c3a8  %s\n' "$OPENTOFU_ARCHIVE" > "$OPENTOFU_CHECKSUM"
sha256sum --check "$OPENTOFU_CHECKSUM"
mkdir -p -- "$OPENTOFU_DIRECTORY"
unzip -q "$OPENTOFU_ARCHIVE" tofu -d "$OPENTOFU_DIRECTORY"
printf '%s\n' "$OPENTOFU_DIRECTORY" >> "$GITHUB_PATH"
"$OPENTOFU_DIRECTORY/tofu" version
