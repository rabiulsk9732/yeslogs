#!/usr/bin/env bash
# Export one day of flow data from ClickHouse to the configured S3 bucket.
#
# Usage: scripts/archive-day.sh YYYY-MM-DD [config]
#
# Uploads to:
#   <path_prefix>/isp_id=<id>/year=YYYY/month=MM/day=DD/part-000.csv.gz
# (parquet falls back to csv.gz in v0.3.1; the s3.export_format config is kept).
set -euo pipefail
cd "$(dirname "$0")/.."

DAY=${1:?usage: archive-day.sh YYYY-MM-DD [config]}
CONFIG=${2:-configs/collector.yaml}
BIN=${BIN:-./bin/natflow-collector}

if [[ ! -x "$BIN" ]]; then
  echo "==> building collector"
  ./scripts/build.sh
fi

echo "==> archiving ${DAY} using ${CONFIG}"
exec "$BIN" --archive-day "$DAY" --config "$CONFIG"
