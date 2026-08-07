#!/usr/bin/env bash
# clean-broken-husks.sh — drop 0-byte broken-* detached parts (empty part dirs
# left behind by an unclean shutdown). These husks carry NO data; removing them
# keeps the broken-parts signal clean so a REAL broken part (bytes > 0) stands
# out. Never touches parts that contain data — those need investigation, not
# deletion. Run on the box itself (needs clickhouse client access).
set -euo pipefail

mapfile -t husks < <(clickhouse client -q \
    "SELECT database, table, name FROM system.detached_parts WHERE name LIKE 'broken%' AND bytes_on_disk = 0 FORMAT TSV")

if [[ ${#husks[@]} -eq 0 ]]; then
    echo "no empty broken-part husks — nothing to do"
    exit 0
fi

echo "dropping ${#husks[@]} empty broken-part husks..."
dropped=0
for line in "${husks[@]}"; do
    IFS=$'\t' read -r db tbl part <<<"$line"
    if clickhouse client -q "ALTER TABLE \`$db\`.\`$tbl\` DROP DETACHED PART '$part' SETTINGS allow_drop_detached = 1"; then
        dropped=$((dropped + 1))
    else
        echo "FAILED: $db.$tbl $part" >&2
    fi
done
echo "dropped $dropped/${#husks[@]}"
remaining=$(clickhouse client -q "SELECT count() FROM system.detached_parts WHERE name LIKE 'broken%'")
echo "broken-* detached parts remaining (any size): $remaining"
