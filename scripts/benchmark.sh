#!/usr/bin/env bash
# Run a NetFlow v5 load ladder against the collector using benchgen.
#
# Env overrides: TARGET, DURATION, FLOWS_PER_PACKET, DNS_PCT, PRIVATE_PCT,
# ZERO_PCT, BENCHGEN, GO.
set -euo pipefail
cd "$(dirname "$0")/.."

TARGET=${TARGET:-127.0.0.1:2055}
DURATION=${DURATION:-60s}
FLOWS_PER_PACKET=${FLOWS_PER_PACKET:-30}
DNS_PCT=${DNS_PCT:-30}
PRIVATE_PCT=${PRIVATE_PCT:-20}
ZERO_PCT=${ZERO_PCT:-5}
BENCHGEN=${BENCHGEN:-./bin/benchgen}

if [[ ! -x "$BENCHGEN" ]]; then
  echo "==> building benchgen"
  CGO_ENABLED=0 "${GO:-go}" build -o ./bin/benchgen ./cmd/benchgen
fi

# pps:senders rungs (more senders so the generator isn't the bottleneck).
ladder=("1000:1" "5000:1" "10000:2" "15000:2" "20000:3")

echo "benchmark ladder -> $TARGET  (duration=$DURATION/rung, ${FLOWS_PER_PACKET} flows/pkt, mix dns=${DNS_PCT}%% priv=${PRIVATE_PCT}%% zero=${ZERO_PCT}%%)"
echo "tip: in another terminal watch the collector ->"
echo "  watch -n1 \"curl -s 127.0.0.1:9101/metrics | grep -E '^(packets_received|flows_decoded|flows_inserted|flows_dropped|current_queue_size)'\""
echo

for entry in "${ladder[@]}"; do
  pps=${entry%%:*}
  senders=${entry##*:}
  cmd=("$BENCHGEN" --target "$TARGET" --pps "$pps" --senders "$senders"
       --flows-per-packet "$FLOWS_PER_PACKET" --duration "$DURATION"
       --dns-percent "$DNS_PCT" --private-percent "$PRIVATE_PCT" --zero-byte-percent "$ZERO_PCT")
  echo "==> ${cmd[*]}"
  "${cmd[@]}"
  echo
done
echo "ladder complete"
