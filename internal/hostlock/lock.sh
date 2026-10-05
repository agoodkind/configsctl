#!/usr/bin/env bash
# Arguments: <operation> <run id> <controller> <ttl seconds> <lock directory>
# Exit 3: a current lock of another run exists. Exit 4: the lock file names
# another run.
set -euo pipefail

OPERATION=$1
RUN=$2
CONTROLLER=$3
TTL=$4
LOCK_DIR=$5
LOCK_FILE="$LOCK_DIR/deploy.lock"
GUARD_DIR="$LOCK_DIR/deploy.lock.guard"
GUARD_WAIT_SECONDS=30

take_guard() {
    local waited=0
    mkdir -p "$LOCK_DIR"
    until mkdir "$GUARD_DIR" 2>/dev/null; do
        if [[ "$waited" -ge "$GUARD_WAIT_SECONDS" ]]; then
            echo "guard $GUARD_DIR stayed busy for $GUARD_WAIT_SECONDS seconds" >&2
            exit 5
        fi
        sleep 1
        waited=$((waited + 1))
    done
    trap 'rmdir "$GUARD_DIR"' EXIT
}

write_lock() {
    local current_time=$1
    printf '%s %s %s\n' "$RUN" "$CONTROLLER" $((current_time + TTL)) > "$LOCK_FILE.tmp"
    mv "$LOCK_FILE.tmp" "$LOCK_FILE"
}

main() {
    local current_time holder="" holder_controller="" expiry=0
    take_guard
    current_time=$(date +%s)
    if [[ -f "$LOCK_FILE" ]]; then
        read -r holder holder_controller expiry < "$LOCK_FILE" || true
    fi
    case "$OPERATION" in
        acquire)
            if [[ -n "$holder" && "$holder" != "$RUN" && "$expiry" -gt "$current_time" ]]; then
                echo "$holder $holder_controller $expiry"
                exit 3
            fi
            write_lock "$current_time"
            ;;
        renew)
            if [[ "$holder" != "$RUN" ]]; then
                echo "$holder $holder_controller $expiry"
                exit 4
            fi
            write_lock "$current_time"
            ;;
        release)
            if [[ "$holder" == "$RUN" ]]; then
                rm -f "$LOCK_FILE"
            fi
            ;;
        unlock)
            if [[ "$holder" != "$RUN" ]]; then
                echo "$holder $holder_controller $expiry"
                exit 4
            fi
            rm -f "$LOCK_FILE"
            ;;
        *)
            echo "unknown operation $OPERATION" >&2
            exit 2
            ;;
    esac
}

main
