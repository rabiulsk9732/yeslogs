#!/usr/bin/env bash
# Restrict the collector's NetFlow/IPFIX UDP ports to a single exporter IP.
#
# SAFE BY DEFAULT: with no DEVICE_IP it only PRINTS the commands and changes
# nothing. Provide DEVICE_IP to actually apply the rules.
#
#   DEVICE_IP=203.0.113.7 ./deploy/ufw-example.sh      # apply
#   ./deploy/ufw-example.sh 203.0.113.7                # apply
#   ./deploy/ufw-example.sh                            # dry-run (print only)
set -euo pipefail

PORTS=(2055 9995 4739)
DEVICE_IP=${1:-${DEVICE_IP:-}}

if [[ -z "$DEVICE_IP" ]]; then
  echo "DRY RUN — no DEVICE_IP given, firewall NOT modified."
  echo "To restrict access to a single exporter, run:"
  echo
  for p in "${PORTS[@]}"; do
    echo "  ufw allow from <DEVICE_IP> to any port $p proto udp"
    echo "  ufw deny $p/udp"
  done
  echo
  echo "Then: DEVICE_IP=<exporter-ip> $0"
  exit 0
fi

echo "Restricting UDP ${PORTS[*]} to exporter ${DEVICE_IP} ..."
for p in "${PORTS[@]}"; do
  echo "+ ufw allow from ${DEVICE_IP} to any port $p proto udp"
  ufw allow from "${DEVICE_IP}" to any port "$p" proto udp
  echo "+ ufw deny $p/udp"
  ufw deny "$p"/udp
done
echo "done — verify with: ufw status numbered"
