#!/usr/bin/env bash
# Deploy mwan binary to a target host with correct mode (0755) and atomic
# replacement, then restart specified services.
#
# Why this exists: scp defaults to mode 0644 on the destination and `mv`
# preserves source mode, so the naive "scp file remote:dest.new && mv dest.new
# dest" pattern silently produces a non-executable binary that crash-loops
# under systemd. Use install(1) semantics: copy to a tmp file, then atomic
# rename with mode 0755 baked in.
#
# Usage:
#   deploy-mwan-binary.sh <target> [service ...]
#
# Targets:
#   ssh:<user@host>          deploy via direct ssh+scp (e.g. ssh:root@vault)
#   lxc:<host>:<vmid>        deploy via pct push on a Proxmox host
#                            (e.g. lxc:root@vault:116)
#
# Examples:
#   deploy-mwan-binary.sh ssh:root@3d06:bad:b01::113 mwan-agent
#   deploy-mwan-binary.sh ssh:root@3d06:bad:b01::254 mwan-ifmgr mwan-watchdog
#   deploy-mwan-binary.sh lxc:root@3d06:bad:b01::254:116 mwan-ifmgr mwan-agent

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_LOCAL="$REPO_ROOT/mwan/go/bin/mwan-linux"
BIN_DEST="/usr/local/bin/mwan"
TMP_DEST="${BIN_DEST}.new"

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <target> [service ...]" >&2
    exit 2
fi

target="$1"
shift
services=("$@")

# bracket_host: ssh accepts bare IPv6 in user@host but scp does not. Accept
# either form from the caller and emit a properly-bracketed user@[v6]:path.
# Inputs we expect: "user@host", "host", "user@[v6]", "user@v6".
bracket_host() {
    local h="$1"
    local user=""
    if [[ "$h" == *@* ]]; then
        user="${h%%@*}@"
        h="${h#*@}"
    fi
    # Already bracketed.
    if [[ "$h" == \[*\] ]]; then
        echo "${user}${h}"
        return
    fi
    # Detect IPv6 by presence of colon (hostnames and IPv4 don't have colons).
    if [[ "$h" == *:* ]]; then
        echo "${user}[${h}]"
        return
    fi
    echo "${user}${h}"
}

if [[ ! -x "$BIN_LOCAL" ]]; then
    echo "error: $BIN_LOCAL not found or not executable; run \`make build-linux\` in mwan/go first" >&2
    exit 1
fi

local_md5=$(md5sum "$BIN_LOCAL" | awk '{print $1}')
echo "[deploy-mwan-binary] local md5: $local_md5"

case "$target" in
    ssh:*)
        host="${target#ssh:}"
        scp_host=$(bracket_host "$host")
        echo "[deploy-mwan-binary] target: direct ssh ($host)"
        # -p preserves mode + mtime so the dest already has 0755 from the local
        # +x bit. install(1) below is belt-and-suspenders.
        scp -p -o ConnectTimeout=15 "$BIN_LOCAL" "${scp_host}:${TMP_DEST}"
        ssh -o ConnectTimeout=15 "$host" \
            "set -e; \
             chmod 0755 ${TMP_DEST}; \
             install -m 0755 -p -T ${TMP_DEST} ${BIN_DEST}; \
             rm -f ${TMP_DEST}; \
             remote_md5=\$(md5sum ${BIN_DEST} | awk '{print \$1}'); \
             test \"\$remote_md5\" = \"${local_md5}\" || { echo \"md5 mismatch: \$remote_md5\" >&2; exit 1; }; \
             test -x ${BIN_DEST} || { echo \"not executable\" >&2; exit 1; }; \
             echo \"[remote] ok: md5 \$remote_md5, mode \$(stat -c %a ${BIN_DEST})\""
        for svc in "${services[@]}"; do
            echo "[deploy-mwan-binary] restarting ${svc} on ${host}"
            ssh -o ConnectTimeout=15 "$host" "systemctl restart ${svc}"
        done
        # Wait briefly and verify each service is active
        if [[ ${#services[@]} -gt 0 ]]; then
            sleep 5
            for svc in "${services[@]}"; do
                state=$(ssh -o ConnectTimeout=15 "$host" "systemctl is-active ${svc}" || true)
                echo "[deploy-mwan-binary] ${svc}: ${state}"
                if [[ "$state" != "active" ]]; then
                    echo "error: ${svc} on ${host} is not active (state=${state})" >&2
                    exit 1
                fi
            done
        fi
        ;;
    lxc:*)
        rest="${target#lxc:}"
        host="${rest%:*}"
        vmid="${rest##*:}"
        if [[ -z "$host" || -z "$vmid" || "$host" == "$vmid" ]]; then
            echo "error: lxc target must be lxc:<host>:<vmid>, got: $target" >&2
            exit 2
        fi
        scp_host=$(bracket_host "$host")
        echo "[deploy-mwan-binary] target: lxc on ${host} vmid=${vmid}"
        # Stage on the Proxmox host first (preserves mode via -p), then push
        # into the container with --perms so pct doesn't reset to 0644.
        host_tmp="/tmp/mwan-deploy-$$.bin"
        scp -p -o ConnectTimeout=15 "$BIN_LOCAL" "${scp_host}:${host_tmp}"
        ssh -o ConnectTimeout=15 "$host" \
            "set -e; \
             chmod 0755 ${host_tmp}; \
             pct push ${vmid} ${host_tmp} ${TMP_DEST} --perms 0755; \
             pct exec ${vmid} -- sh -c 'install -m 0755 -p -T ${TMP_DEST} ${BIN_DEST} && rm -f ${TMP_DEST} && remote_md5=\$(md5sum ${BIN_DEST} | awk \"{print \\\$1}\") && test \"\$remote_md5\" = \"${local_md5}\" || { echo md5 mismatch: \$remote_md5 >&2; exit 1; } && test -x ${BIN_DEST} || { echo not executable >&2; exit 1; } && echo \"[lxc ${vmid}] ok: md5 \$remote_md5, mode \$(stat -c %a ${BIN_DEST})\"'; \
             rm -f ${host_tmp}"
        for svc in "${services[@]}"; do
            echo "[deploy-mwan-binary] restarting ${svc} in lxc ${vmid}"
            ssh -o ConnectTimeout=15 "$host" "pct exec ${vmid} -- systemctl restart ${svc}"
        done
        if [[ ${#services[@]} -gt 0 ]]; then
            sleep 5
            for svc in "${services[@]}"; do
                state=$(ssh -o ConnectTimeout=15 "$host" "pct exec ${vmid} -- systemctl is-active ${svc}" || true)
                echo "[deploy-mwan-binary] lxc ${vmid} ${svc}: ${state}"
                if [[ "$state" != "active" ]]; then
                    echo "error: ${svc} in lxc ${vmid} is not active (state=${state})" >&2
                    exit 1
                fi
            done
        fi
        ;;
    *)
        echo "error: unknown target prefix; expected ssh:... or lxc:..., got: $target" >&2
        exit 2
        ;;
esac

echo "[deploy-mwan-binary] done"
