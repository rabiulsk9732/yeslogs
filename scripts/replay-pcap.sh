#!/usr/bin/env bash
# Replay a (sanitized) pcap into a collector. Wrapper around cmd/pcapreplay.
#
# Usage: replay-pcap.sh <pcap> [target] [speed] [proto]
#   defaults: target=127.0.0.1:2055 speed=1x proto=auto
set -euo pipefail
cd "$(dirname "$0")/.."

PCAP=${1:?usage: replay-pcap.sh <pcap> [target] [speed] [proto]}
TARGET=${2:-127.0.0.1:2055}
SPEED=${3:-1x}
PROTO=${4:-auto}
BIN=${BIN:-./bin/pcapreplay}

if [[ ! -x "$BIN" ]]; then
  echo "==> building pcapreplay"
  CGO_ENABLED=0 "${GO:-go}" build -o ./bin/pcapreplay ./cmd/pcapreplay
fi

exec "$BIN" --pcap "$PCAP" --target "$TARGET" --speed "$SPEED" --proto "$PROTO"
