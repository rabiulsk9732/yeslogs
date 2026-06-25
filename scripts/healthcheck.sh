#!/usr/bin/env bash
# Health check for natflow-collector. Runs all checks and exits non-zero if any
# fail (suitable for cron / monitoring).
#
# Env overrides: SERVICE, METRICS, CH_CLIENT, CH_HOST, SKIP_SYSTEMD=1.
set -uo pipefail

SERVICE=${SERVICE:-natflow-collector}
METRICS=${METRICS:-127.0.0.1:9101}
CH_CLIENT=${CH_CLIENT:-clickhouse-client}
CH_HOST=${CH_HOST:-127.0.0.1}
SKIP_SYSTEMD=${SKIP_SYSTEMD:-0}

rc=0
ok()  { echo "  OK   $*"; }
bad() { echo "  FAIL $*"; rc=1; }

echo "natflow-dataplane healthcheck"

# 1. systemd service
if [[ "$SKIP_SYSTEMD" == "1" ]]; then
  echo "  SKIP systemd check (SKIP_SYSTEMD=1)"
elif command -v systemctl >/dev/null 2>&1; then
  if systemctl is-active --quiet "$SERVICE"; then
    ok "systemd service '$SERVICE' active"
  else
    bad "systemd service '$SERVICE' not active"
  fi
else
  bad "systemctl not found"
fi

# 2. metrics endpoint liveness
if curl -fsS --max-time 3 "http://$METRICS/healthz" >/dev/null 2>&1; then
  ok "metrics endpoint http://$METRICS/healthz reachable"
else
  bad "metrics endpoint http://$METRICS/healthz unreachable"
fi

# 3. ClickHouse reachable + data flowing today
if command -v "$CH_CLIENT" >/dev/null 2>&1; then
  if out=$("$CH_CLIENT" --host "$CH_HOST" -q \
      "SELECT count() FROM natlogs.flow_logs WHERE event_date >= today()" 2>/dev/null); then
    ok "clickhouse reachable; rows today = ${out}"
  else
    bad "clickhouse query failed"
  fi
else
  bad "$CH_CLIENT not found"
fi

if [[ $rc -eq 0 ]]; then echo "RESULT: healthy"; else echo "RESULT: UNHEALTHY"; fi
exit $rc
