#!/usr/bin/env bash
# fleet-check.sh — compare natlog posture across all boxes and flag drift.
#
# Reads box definitions from /etc/natlog/fleet.conf (kept OUT of git — it
# names production hosts). One box per line:
#
#     <name>              (empty command = this box, run locally)
#     <name> <ssh command prefix ...>
#
# e.g.:
#     box1
#     box2 ssh -p 1245 -i /root/.ssh/box2_admin1 -o BatchMode=yes admin1@160.30.85.14
#
# For every box it reports: service states, natlog binary fingerprint, the
# ClickHouse broken-parts guard, broken detached parts, timezone, ingest
# liveness (rows today + days present in the last 14), backup timer, disk.
# Exit code 1 if any box is unreachable or any RED condition is found.
set -u

CONF="${FLEET_CONF:-/etc/natlog/fleet.conf}"
if [[ ! -r "$CONF" ]]; then
    echo "fleet-check: no $CONF — create it (see header comment). Checking local box only." >&2
    CONF=""
fi

# probe <ssh-prefix...> -- <script>   (empty prefix = local)
probe() {
    local -a pre=()
    while [[ $# -gt 0 && "$1" != "--" ]]; do pre+=("$1"); shift; done
    shift
    if [[ ${#pre[@]} -eq 0 ]]; then
        bash -c "$1" 2>/dev/null
    else
        # </dev/null: ssh must not swallow the caller's stdin (the conf file
        # being read by the box loop)
        "${pre[@]}" "$1" 2>/dev/null </dev/null
    fi
}

RED=0
declare -A BINHASH GUARD TZ

# One remote round-trip per box: every fact on its own KEY=VALUE line.
FACTS='
echo "natlog=$(systemctl is-active natlog 2>/dev/null)"
echo "clickhouse=$(systemctl is-active clickhouse-server 2>/dev/null)"
echo "mariadb=$(systemctl is-active mariadb 2>/dev/null)"
echo "backup_timer=$(systemctl is-active natflow-backup.timer 2>/dev/null)"
echo "bin=$(sha256sum /usr/local/bin/natlog 2>/dev/null | cut -c1-12)"
echo "bin_date=$(date -r /usr/local/bin/natlog +%F 2>/dev/null)"
echo "guard=$(clickhouse client -q "SELECT value FROM system.merge_tree_settings WHERE name='"'"'max_suspicious_broken_parts'"'"'" 2>/dev/null)"
echo "broken=$(clickhouse client -q "SELECT count() FROM system.detached_parts WHERE name LIKE '"'"'broken%'"'"' AND bytes_on_disk > 0" 2>/dev/null)"
echo "husks=$(clickhouse client -q "SELECT count() FROM system.detached_parts WHERE name LIKE '"'"'broken%'"'"' AND bytes_on_disk = 0" 2>/dev/null)"
echo "tz=$(clickhouse client -q "SELECT timezone()" 2>/dev/null)"
echo "rows_today=$(clickhouse client -q "SELECT count() FROM natlogs.flow_logs WHERE event_date=today()" 2>/dev/null)"
echo "days_14=$(clickhouse client -q "SELECT uniqExact(event_date) FROM natlogs.flow_logs WHERE event_date >= today()-13" 2>/dev/null)"
echo "disk=$(df -h --output=pcent / 2>/dev/null | tail -1 | tr -d " ")"
'

check_box() {
    local name="$1"; shift
    local -a pre=("$@")
    local out
    out="$(probe "${pre[@]}" -- "$FACTS")"
    if [[ -z "$out" ]]; then
        printf '%-6s UNREACHABLE\n' "$name"
        RED=1
        return
    fi
    local natlog ch maria timer bin bin_date guard broken husks tz rows days disk
    natlog=$(sed -n 's/^natlog=//p' <<<"$out");      ch=$(sed -n 's/^clickhouse=//p' <<<"$out")
    maria=$(sed -n 's/^mariadb=//p' <<<"$out");      timer=$(sed -n 's/^backup_timer=//p' <<<"$out")
    bin=$(sed -n 's/^bin=//p' <<<"$out");            bin_date=$(sed -n 's/^bin_date=//p' <<<"$out")
    guard=$(sed -n 's/^guard=//p' <<<"$out");        broken=$(sed -n 's/^broken=//p' <<<"$out")
    husks=$(sed -n 's/^husks=//p' <<<"$out")
    tz=$(sed -n 's/^tz=//p' <<<"$out");              rows=$(sed -n 's/^rows_today=//p' <<<"$out")
    days=$(sed -n 's/^days_14=//p' <<<"$out");       disk=$(sed -n 's/^disk=//p' <<<"$out")

    BINHASH[$name]="$bin"; GUARD[$name]="$guard"; TZ[$name]="$tz"

    local flags=""
    [[ "$natlog" != "active" || "$ch" != "active" || "$maria" != "active" ]] && { flags+=" [RED service down]"; RED=1; }
    [[ -n "$broken" && "$broken" != "0" ]] && { flags+=" [RED $broken broken parts WITH DATA — rows lost from hot storage]"; RED=1; }
    [[ -n "$husks" && "$husks" != "0" ]] && flags+=" [WARN $husks empty broken-part husks — crash debris, clean up]"
    [[ -z "$rows" || "$rows" == "0" ]] && { flags+=" [RED no rows today]"; RED=1; }
    [[ -n "$days" && "$days" -lt 14 ]] && flags+=" [WARN only $days/14 days present — gap in hot data]"
    [[ "$timer" != "active" ]] && flags+=" [WARN backup timer inactive]"
    [[ -n "$guard" && "$guard" -lt 1000 ]] && flags+=" [WARN broken-parts guard low ($guard)]"
    [[ "$tz" != "Asia/Kolkata" ]] && flags+=" [WARN tz=$tz not IST]"

    printf '%-6s natlog=%s ch=%s db=%s backup=%s | bin=%s(%s) guard=%s broken=%s husks=%s | today=%s days14=%s disk=%s%s\n' \
        "$name" "$natlog" "$ch" "$maria" "$timer" "${bin:-?}" "${bin_date:-?}" "${guard:-?}" "${broken:-?}" "${husks:-?}" \
        "${rows:-?}" "${days:-?}" "${disk:-?}" "$flags"
}

if [[ -n "$CONF" ]]; then
    while read -r name rest; do
        [[ -z "$name" || "$name" == \#* ]] && continue
        # shellcheck disable=SC2086
        check_box "$name" $rest
    done < "$CONF"
else
    check_box local
fi

# Drift summary: every box should run the same binary and the same guard.
if [[ ${#BINHASH[@]} -gt 1 ]]; then
    if [[ $(printf '%s\n' "${BINHASH[@]}" | sort -u | wc -l) -gt 1 ]]; then
        echo "DRIFT: natlog binaries differ across boxes — redeploy the fleet to one build."
    fi
    if [[ $(printf '%s\n' "${GUARD[@]}" | sort -u | wc -l) -gt 1 ]]; then
        echo "DRIFT: max_suspicious_broken_parts differs across boxes."
    fi
    if [[ $(printf '%s\n' "${TZ[@]}" | sort -u | wc -l) -gt 1 ]]; then
        echo "DRIFT: ClickHouse timezone differs across boxes."
    fi
fi

exit $RED
