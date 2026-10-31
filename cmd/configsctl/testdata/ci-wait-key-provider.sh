#!/usr/bin/env sh
# TestCITofuStopsOpenTofuOnSignals uses this script as an external key provider.
# The script writes the OpenTofu process ID and its own process ID to the path
# in its first argument, then replaces itself with sleep for 300 seconds.
set -eu
PID_FILE=$1
printf '%s %s\n' "$PPID" "$$" > "$PID_FILE.partial"
mv "$PID_FILE.partial" "$PID_FILE"
exec sleep 300
