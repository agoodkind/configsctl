#!/usr/bin/env sh
# TestRunStopsTheProcessGroupWhenTheContextEnds runs this script from a
# local-exec provisioner. The script starts sleep in the background, writes the
# sleep process ID to the path in its first argument, and waits for sleep.
set -eu
PID_FILE=$1
sleep 300 &
printf '%s\n' "$!" > "$PID_FILE.partial"
mv "$PID_FILE.partial" "$PID_FILE"
wait
