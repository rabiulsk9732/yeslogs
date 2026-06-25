#!/usr/bin/env bash
# Capture NetFlow/IPFIX UDP traffic from a real exporter for later replay.
# Captures UDP ports 2055 (v5), 9995 (v9) and 4739 (IPFIX).
#
# Usage: capture-device.sh <interface> <output.pcap> <duration_seconds>
# Requires: tcpdump (run as root).
set -euo pipefail

IFACE=${1:?usage: capture-device.sh <interface> <output.pcap> <duration_seconds>}
OUT=${2:?usage: capture-device.sh <interface> <output.pcap> <duration_seconds>}
DUR=${3:?usage: capture-device.sh <interface> <output.pcap> <duration_seconds>}

echo "==> capturing UDP 2055/9995/4739 on ${IFACE} for ${DUR}s -> ${OUT}"
tcpdump -i "$IFACE" -w "$OUT" -G "$DUR" -W 1 \
  'udp port 2055 or udp port 9995 or udp port 4739'

cat <<EOF

############################################################
# WARNING: ${OUT} contains REAL exporter and subscriber IPs.
# Sanitize BEFORE sharing or committing it:
#
#   pcapsanitize --in "${OUT}" --out "${OUT%.pcap}-sanitized.pcap"
#
# Never commit an unsanitized capture of customer traffic.
############################################################
EOF
