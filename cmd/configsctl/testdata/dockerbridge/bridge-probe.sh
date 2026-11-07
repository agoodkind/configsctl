#!/usr/bin/env bash

set -euo pipefail

probe() {
  local socket="${1#unix://}"
  curl --silent --output /dev/null --max-time 30 --write-out '%{http_code}' \
    --unix-socket "${socket}" --request POST http://localhost/volumes/create
}

umask 077
{
  printf 'guest_alpha=%s\n' "$(probe "${CONFIGSCTL_DOCKER_ALPHA:?}")"
  printf 'guest_beta=%s\n' "$(probe "${CONFIGSCTL_DOCKER_BETA:?}")"
} > "${CONFIGSCTL_DOCKER_RESULT:?}"
