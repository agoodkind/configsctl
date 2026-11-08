#!/usr/bin/env bash

set -euo pipefail

marker="${CONFIGSCTL_LIVE_MARKER:?}"
umask 077
printf '%s\n' "$$" > "${marker}.tmp"
mv "${marker}.tmp" "${marker}"
exec sleep 600
