#!/usr/bin/env bash

set -euo pipefail

umask 077
printf '%s\n' "${CONFIGSCTL_EPHEMERAL_SESSION:?}" > "${CONFIGSCTL_EPHEMERAL_RESULT:?}"
