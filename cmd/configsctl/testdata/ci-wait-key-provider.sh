#!/usr/bin/env sh
# TestCITofuStopsOpenTofuOnSignals uses this script as an external key provider.
# The script writes the OpenTofu process ID and its own process ID to the path
# in its first argument, then replaces itself with sleep for 300 seconds.
set -eu
pid_file=$1
printf '%s %s\n' "$PPID" "$$" > "$pid_file.partial"
mv "$pid_file.partial" "$pid_file"
exec sleep 300
