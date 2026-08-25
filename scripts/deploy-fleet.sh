#!/usr/bin/env bash
# deploy-fleet.sh — build once, deploy the natlog binary to every box in
# /etc/natlog/fleet.conf (same format as fleet-check.sh), restart, verify.
#
# Run AS THE OPERATOR from the build box (box1):
#   sudo bash scripts/deploy-fleet.sh            # deploy committed HEAD
#   BIN=/path/to/natlog sudo bash scripts/...    # deploy a prebuilt binary
#
# Remote boxes: the ssh prefix from fleet.conf is used; install runs via
# sudo on the remote side (you'll be prompted for the sudo password unless
# passwordless). Local box (no ssh prefix): plain install + restart.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
GO="${GO:-/usr/local/go/bin/go}"
CONF="${FLEET_CONF:-/etc/natlog/fleet.conf}"

if [[ -z "${BIN:-}" ]]; then
    echo "==> building natlog from $(git -C "$REPO" rev-parse --short HEAD)"
    (cd "$REPO" && "$GO" build -o bin/natlog ./cmd/natlog)
    BIN="$REPO/bin/natlog"
fi
HASH=$(sha256sum "$BIN" | cut -c1-12)
echo "==> deploying $BIN ($HASH)"

deploy_local() {
    install -m0755 "$BIN" /usr/local/bin/natlog
    systemctl restart natlog
}

deploy_remote() {
    local -a pre=("$@")
    # last arg of the prefix is user@host; everything else are ssh options
    local target="${pre[-1]}"
    local -a opts=("${pre[@]:1:${#pre[@]}-2}")   # drop leading "ssh" + trailing target
    scp "${opts[@]/#-p/-P}" "$BIN" "$target:/tmp/natlog.new"
    ssh -t "${opts[@]}" "$target" \
        "sudo install -m0755 /tmp/natlog.new /usr/local/bin/natlog && sudo systemctl restart natlog && rm -f /tmp/natlog.new"
}

verify() {
    local name="$1"; shift
    local -a pre=("$@")
    local got
    if [[ ${#pre[@]} -eq 0 ]]; then
        got=$( { systemctl is-active natlog; sha256sum /usr/local/bin/natlog | cut -c1-12; } | paste -sd' ')
    else
        got=$("${pre[@]}" 'systemctl is-active natlog; sha256sum /usr/local/bin/natlog | cut -c1-12' </dev/null | paste -sd' ')
    fi
    if [[ "$got" == "active $HASH" ]]; then
        echo "    $name OK: active, $HASH"
    else
        echo "    $name MISMATCH: $got (wanted: active $HASH)"
        return 1
    fi
}

FAIL=0
while read -r name rest; do
    [[ -z "$name" || "$name" == \#* ]] && continue
    echo "==> $name"
    # shellcheck disable=SC2086
    set -- $rest
    if [[ $# -eq 0 ]]; then
        deploy_local || { echo "    $name deploy FAILED"; FAIL=1; continue; }
    else
        deploy_remote "$@" || { echo "    $name deploy FAILED"; FAIL=1; continue; }
    fi
    sleep 3
    verify "$name" "$@" || FAIL=1
done < "$CONF"

exit $FAIL
