#!/usr/bin/env bash
# The script accepts <operation>, <run id>, <controller>, <lease seconds>,
# <lock directory>, and <extend cap seconds>.
# The script exits with status 3 when another run owns a current lock.
# The script exits with status 4 when the lock file records another owner.
# extend exits with status 6 when the requested lease exceeds the cap.
# acquire, renew, and extend print "<expiry> <host time>" in epoch seconds.
set -euo pipefail

OPERATION=$1
RUN=$2
CONTROLLER=$3
TTL=$4
LOCK_DIR=$5
EXTEND_CAP=$6
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
    local current_time=$1 new_expiry=$2
    printf '%s %s %s\n' "$RUN" "$CONTROLLER" "$new_expiry" > "$LOCK_FILE.tmp"
    mv "$LOCK_FILE.tmp" "$LOCK_FILE"
    echo "$new_expiry $current_time"
}

# renew and extend never write an expiry earlier than the stored expiry.
write_later_expiry() {
    local current_time=$1 stored_expiry=$2 new_expiry
    new_expiry=$((current_time + TTL))
    if [[ "$stored_expiry" -gt "$new_expiry" ]]; then
        new_expiry=$stored_expiry
    fi
    write_lock "$current_time" "$new_expiry"
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
            write_lock "$current_time" $((current_time + TTL))
            ;;
        renew)
            if [[ "$holder" != "$RUN" ]]; then
                echo "$holder $holder_controller $expiry"
                exit 4
            fi
            write_later_expiry "$current_time" "$expiry"
            ;;
        extend)
            if [[ "$TTL" -lt 1 || "$TTL" -gt "$EXTEND_CAP" ]]; then
                echo "extend of $TTL seconds is outside 1 to $EXTEND_CAP seconds" >&2
                exit 6
            fi
            if [[ "$holder" != "$RUN" ]]; then
                echo "$holder $holder_controller $expiry"
                exit 4
            fi
            write_later_expiry "$current_time" "$expiry"
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
