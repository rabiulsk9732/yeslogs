#!/usr/bin/env bash
# Production install for the unified NATLog service (dataplane + control plane)
# with systemd. Sets up ClickHouse + MariaDB + natlog as managed services.
#
#   sudo ./scripts/install.sh
#
# Env:
#   CLICKHOUSE_BIN=/path/to/clickhouse   # source for the clickhouse binary if
#                                        # /usr/local/bin/clickhouse is absent
set -euo pipefail
cd "$(dirname "$0")/.."

[[ ${EUID:-$(id -u)} -eq 0 ]] || { echo "error: run as root" >&2; exit 1; }

ETC_NATLOG=/etc/natlog
ETC_CH=/etc/clickhouse-server
CH_DATA=/var/lib/clickhouse
CH_LOG=/var/log/clickhouse-server
NATLOG_CFG="$ETC_NATLOG/natlog.yaml"
CRED_FILE="$ETC_NATLOG/admin-credentials.txt"

rand() { head -c "${1:-24}" /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "${1:-24}"; }

echo "==> build"
./scripts/build.sh

echo "==> system users"
id -u clickhouse >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin clickhouse
id -u natlog     >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin natlog

echo "==> binaries -> /usr/local/bin"
install -m 0755 bin/natlog /usr/local/bin/natlog
if [[ ! -x /usr/local/bin/clickhouse ]]; then
  if [[ -n "${CLICKHOUSE_BIN:-}" && -x "${CLICKHOUSE_BIN}" ]]; then
    install -m 0755 "${CLICKHOUSE_BIN}" /usr/local/bin/clickhouse
  else
    echo "error: /usr/local/bin/clickhouse missing. Install ClickHouse (https://clickhouse.com/) or set CLICKHOUSE_BIN=." >&2
    exit 1
  fi
fi

echo "==> ClickHouse config + dirs"
install -d -o clickhouse -g clickhouse "$CH_DATA" "$CH_DATA/tmp" "$CH_DATA/user_files" "$CH_LOG"
install -d "$ETC_CH" "$ETC_CH/config.d"
install -m 0644 deploy/clickhouse/config.xml "$ETC_CH/config.xml"
install -m 0644 deploy/clickhouse/users.xml  "$ETC_CH/users.xml"
[[ -f deploy/clickhouse/natflow-tuning.xml ]] && install -m 0644 deploy/clickhouse/natflow-tuning.xml "$ETC_CH/config.d/natflow-tuning.xml" || true

echo "==> systemd units"
install -m 0644 deploy/systemd/clickhouse-server.service /etc/systemd/system/clickhouse-server.service
install -m 0644 deploy/systemd/natlog.service           /etc/systemd/system/natlog.service
systemctl daemon-reload

echo "==> start MariaDB + ClickHouse"
systemctl enable --now mariadb
# Pin MariaDB to UTC so audit CURRENT_TIMESTAMP is a deterministic instant (the
# console converts to IST for display; ClickHouse is set to Asia/Kolkata).
install -d /etc/mysql/mariadb.conf.d
printf '[mariadbd]\ndefault_time_zone="+00:00"\n' > /etc/mysql/mariadb.conf.d/99-natlog-tz.cnf
mariadb -e "SET GLOBAL time_zone='+00:00'" 2>/dev/null || true
systemctl enable --now clickhouse-server
for i in $(seq 1 30); do /usr/local/bin/clickhouse client --host 127.0.0.1 -q "SELECT 1" >/dev/null 2>&1 && break; sleep 1; done

echo "==> control-plane database (MariaDB)"
if [[ -f "$NATLOG_CFG" ]]; then
  echo "    keeping existing $NATLOG_CFG (secrets preserved)"
else
  DBPW=$(rand 24); SK=$(rand 40)
  mariadb -e "CREATE DATABASE IF NOT EXISTS natflow_cp;
    CREATE USER IF NOT EXISTS 'natflow'@'127.0.0.1' IDENTIFIED BY '${DBPW}';
    ALTER USER 'natflow'@'127.0.0.1' IDENTIFIED BY '${DBPW}';
    GRANT ALL PRIVILEGES ON natflow_cp.* TO 'natflow'@'127.0.0.1'; FLUSH PRIVILEGES;"
  install -d -m 0750 -o root -g natlog "$ETC_NATLOG"
  umask 027
  sed -e "s#^  session_key:.*#  session_key: \"${SK}\"#" \
      -e "s#^  mysql_dsn:.*#  mysql_dsn: \"natflow:${DBPW}@tcp(127.0.0.1:3306)/natflow_cp?parseTime=true\&loc=UTC\"#" \
      configs/natlog.yaml > "$NATLOG_CFG"
  chown root:natlog "$NATLOG_CFG"; chmod 0640 "$NATLOG_CFG"
  echo "    generated $NATLOG_CFG"
fi

echo "==> ClickHouse schema"
/usr/local/bin/clickhouse client --host 127.0.0.1 --multiquery < migrations/clickhouse.sql

echo "==> control-plane schema + admin"
/usr/local/bin/natlog --config "$NATLOG_CFG" --migrate
if [[ ! -f "$CRED_FILE" ]]; then
  ADMIN_EMAIL=${ADMIN_EMAIL:-admin@natlog.local}; ADMIN_PW=$(rand 18)
  if /usr/local/bin/natlog --config "$NATLOG_CFG" --create-admin --email "$ADMIN_EMAIL" --password "$ADMIN_PW"; then
    umask 077; printf 'admin email: %s\nadmin password: %s\n' "$ADMIN_EMAIL" "$ADMIN_PW" > "$CRED_FILE"
    echo "    admin created; credentials saved to $CRED_FILE"
  fi
fi

if [[ "${START_NATLOG:-1}" == "1" ]]; then
  echo "==> enable + start natlog"
  systemctl enable --now natlog
else
  echo "==> natlog unit installed (not started). Start with: systemctl enable --now natlog"
  systemctl enable natlog >/dev/null 2>&1 || true
fi

IP=$(hostname -I | awk '{print $1}')
BINDPORT=$(awk '/^cp:/{f=1} f&&/^  bind:/{gsub(/[" ]/,"");split($0,a,":");print a[3];exit}' "$NATLOG_CFG")
cat <<EOF

============================================================
 NATLog is installed and running as a systemd service.
   Console:  http://${IP}:${BINDPORT:-8080}
   Creds:    $CRED_FILE   (cat it as root)
 Manage:
   systemctl status natlog clickhouse-server mariadb
   journalctl -u natlog -f
============================================================
EOF
