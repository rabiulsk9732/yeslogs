#!/usr/bin/env bash
# Real disaster-recovery backup of the MariaDB control plane (natflow_cp) — the
# system of record for tenants, users, devices, capture policies, settings, and
# the lawful-access query audit. Flow data itself lives in ClickHouse + the S3
# cold archive; THIS protects the control plane, which has no other backup.
#
# Cron/systemd-timer friendly. Keeps the last RETENTION dumps locally; if an S3
# target is configured it also uploads each dump off-box.
#
#   Env: DB (natflow_cp), BACKUP_DIR (/var/backups/natflow/cp), RETENTION (14),
#        MYSQL_DEFAULTS_FILE (a my.cnf with [client] user/password),
#        S3_DEST (e.g. s3://bucket/prefix) + S3_CLIENT (aws|mc) for off-box copy.
#
# RESTORE:
#   gunzip -c cp-YYYYmmdd-HHMMSS.sql.gz | mariadb natflow_cp
#   (recreate the DB first if needed: mariadb -e "CREATE DATABASE natflow_cp")
#   then: systemctl restart natlog
set -euo pipefail

DB=${DB:-natflow_cp}
BACKUP_DIR=${BACKUP_DIR:-/var/backups/natflow/cp}
RETENTION=${RETENTION:-14}
MYSQL_DEFAULTS_FILE=${MYSQL_DEFAULTS_FILE:-}

mkdir -p "$BACKUP_DIR"; chmod 700 "$BACKUP_DIR"
ts=$(date +%Y%m%d-%H%M%S)
out="$BACKUP_DIR/cp-${ts}.sql.gz"

dumpcmd=(mysqldump --single-transaction --routines --triggers --events)
[ -n "$MYSQL_DEFAULTS_FILE" ] && dumpcmd=(mysqldump "--defaults-file=$MYSQL_DEFAULTS_FILE" --single-transaction --routines --triggers --events)

echo "==> dumping $DB -> $out"
"${dumpcmd[@]}" "$DB" | gzip -9 > "$out"
chmod 600 "$out"
sz=$(du -h "$out" | cut -f1)
rows=$(gunzip -c "$out" | grep -c 'INSERT INTO' || true)
echo "    wrote $out ($sz, ~$rows INSERT statements)"

# Off-box copy (optional but strongly recommended for real DR).
if [ -n "${S3_DEST:-}" ]; then
  case "${S3_CLIENT:-aws}" in
    aws) aws s3 cp "$out" "${S3_DEST%/}/$(basename "$out")" && echo "    uploaded to ${S3_DEST}" ;;
    mc)  mc cp "$out" "${S3_DEST%/}/$(basename "$out")" && echo "    uploaded to ${S3_DEST}" ;;
    *)   echo "    WARN: unknown S3_CLIENT='${S3_CLIENT}', skipped off-box copy" ;;
  esac
else
  echo "    NOTE: set S3_DEST + S3_CLIENT to copy the dump off-box (on-box-only backup does not survive disk loss)"
fi

# Local retention.
ls -1t "$BACKUP_DIR"/cp-*.sql.gz 2>/dev/null | tail -n +$((RETENTION + 1)) | while read -r old; do
  echo "    pruning $old"; rm -f "$old"
done
echo "==> control-plane backup complete"
