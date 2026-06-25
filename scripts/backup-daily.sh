#!/usr/bin/env bash
# Write a daily SUMMARY snapshot of the flow_logs table (not raw data) to a
# timestamped text file. Intended for cron.
#
# Env overrides: BACKUP_DIR, CH_CLIENT, CH_HOST, DB, TABLE.
set -euo pipefail

BACKUP_DIR=${BACKUP_DIR:-/var/backups/natflow}
CH_CLIENT=${CH_CLIENT:-clickhouse-client}
CH_HOST=${CH_HOST:-127.0.0.1}
DB=${DB:-natlogs}
TABLE=${TABLE:-flow_logs}

mkdir -p "$BACKUP_DIR"
ts=$(date +%Y%m%d-%H%M%S)
out="$BACKUP_DIR/natflow-summary-${ts}.txt"

q() { "$CH_CLIENT" --host "$CH_HOST" -q "$1"; }

{
  echo "natflow-dataplane daily summary"
  echo "generated:   $(date -Is)"
  echo "table:       ${DB}.${TABLE}"
  echo
  echo "rows:        $(q "SELECT count() FROM ${DB}.${TABLE}")"
  echo "flow_start:  $(q "SELECT min(flow_start) FROM ${DB}.${TABLE}") .. $(q "SELECT max(flow_start) FROM ${DB}.${TABLE}")"
  echo "total_bytes: $(q "SELECT sum(bytes) FROM ${DB}.${TABLE}")"
  echo "on_disk:     $(q "SELECT formatReadableSize(sum(bytes_on_disk)) FROM system.parts WHERE database='${DB}' AND table='${TABLE}' AND active")"
  echo
  echo "rows by day (last 14):"
  q "SELECT event_date, count() AS rows, formatReadableSize(sum(bytes)) AS bytes
     FROM ${DB}.${TABLE} GROUP BY event_date ORDER BY event_date DESC LIMIT 14
     FORMAT PrettyCompactNoEscapes"
} > "$out"

echo "wrote $out"
