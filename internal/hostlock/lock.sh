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
GUARD_PREFIX="$LOCK_DIR/deploy.lock.guard"
GUARD_WAIT_SECONDS=30

boot_id() {
    local boot_time
    if [[ -r /proc/sys/kernel/random/boot_id ]]; then
        cat /proc/sys/kernel/random/boot_id
        return
    fi
    boot_time=$(sysctl -n kern.boottime)
    boot_time=${boot_time#*sec = }
    echo "${boot_time%%,*}"
}

# A reboot skips the EXIT trap that removes the guard directory.
GUARD_DIR="$GUARD_PREFIX.$(boot_id)"

remove_stale_guards() {
    local guard
    for guard in "$GUARD_PREFIX" "$GUARD_PREFIX".*; do
        if [[ -d "$guard" && "$guard" != "$GUARD_DIR" ]]; then
            rm -rf "$guard"
        fi
    done
}

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
    remove_stale_guards
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
