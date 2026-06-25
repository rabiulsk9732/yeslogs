#!/usr/bin/env bash
# Build and install the collector under /opt/natflow-dataplane with its systemd
# unit. Run as root.
set -euo pipefail

PREFIX=${PREFIX:-/opt/natflow-dataplane}
LOGDIR=${LOGDIR:-/var/log/natflow}
cd "$(dirname "$0")/.."

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  echo "error: must run as root" >&2
  exit 1
fi

echo "==> building"
./scripts/build.sh

echo "==> installing to ${PREFIX}"
install -d "${PREFIX}/bin" "${PREFIX}/configs" "${PREFIX}/migrations" "${LOGDIR}"
install -m 0755 bin/natflow-collector "${PREFIX}/bin/natflow-collector"
install -m 0644 migrations/clickhouse.sql "${PREFIX}/migrations/clickhouse.sql"

if [[ -f "${PREFIX}/configs/collector.yaml" ]]; then
  echo "    keeping existing config at ${PREFIX}/configs/collector.yaml"
else
  install -m 0644 configs/collector.yaml "${PREFIX}/configs/collector.yaml"
  echo "    installed default config -> ${PREFIX}/configs/collector.yaml"
fi

echo "==> installing systemd unit"
install -m 0644 systemd/natflow-collector.service /etc/systemd/system/natflow-collector.service
systemctl daemon-reload

cat <<EOF

Installed. Next steps:
  1. Apply the schema:   clickhouse-client --multiquery < ${PREFIX}/migrations/clickhouse.sql
  2. Review config:      ${PREFIX}/configs/collector.yaml
  3. Enable + start:     systemctl enable --now natflow-collector
  4. Check status/logs:  systemctl status natflow-collector ; journalctl -u natflow-collector -f
EOF
