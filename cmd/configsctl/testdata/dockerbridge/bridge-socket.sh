#!/usr/bin/env bash

set -euo pipefail

socket="${CONFIGSCTL_DOCKER_HOST#unix://}"
if [[ ! -S "${socket}" ]]; then
  printf 'the Docker bridge socket %s does not exist\n' "${socket}" >&2
  exit 1
fi
umask 077
printf '%s\n' "${socket}" > "${CONFIGSCTL_DOCKER_RESULT:?}"
