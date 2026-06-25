#!/usr/bin/env bash
# Send a synthetic NetFlow v5 datagram (2 flows: one normal, one DNS) to the
# collector. Requires python3.
#
# Usage: scripts/test-send.sh [host] [port]
set -euo pipefail

HOST=${1:-127.0.0.1}
PORT=${2:-2055}

python3 - "$HOST" "$PORT" <<'PY'
import socket, struct, sys, time

host, port = sys.argv[1], int(sys.argv[2])
sys_uptime = 100000           # ms since exporter boot
now = int(time.time())

# v5 header: version, count, sys_uptime, unix_secs, unix_nsecs,
#            flow_sequence, engine_type, engine_id, sampling_interval
header = struct.pack("!HHIIIIBBH", 5, 2, sys_uptime, now, 0, 0, 0, 0, 0)

def ipv4(s):
    return socket.inet_aton(s)

def record(src, dst, sp, dp, pkts, octets, first, last, proto, tcp=0x10):
    return (ipv4(src) + ipv4(dst) + ipv4("0.0.0.0")  # src, dst, nexthop
            + struct.pack("!HH", 0, 0)               # input, output snmp ifindex
            + struct.pack("!II", pkts, octets)       # dPkts, dOctets
            + struct.pack("!II", first, last)        # first, last (uptime ms)
            + struct.pack("!HH", sp, dp)             # srcport, dstport
            + struct.pack("!BBBB", 0, tcp, proto, 0) # pad, tcp_flags, prot, tos
            + struct.pack("!HH", 0, 0)               # src_as, dst_as
            + struct.pack("!BB", 24, 24)             # src_mask, dst_mask
            + struct.pack("!H", 0))                  # pad2

normal = record("10.0.0.5", "1.1.1.1", 40000, 443, 10, 1500, sys_uptime-2000, sys_uptime-1000, 6)
dns    = record("10.0.0.5", "8.8.8.8", 40001, 53,   2,  140, sys_uptime-500,  sys_uptime-400, 17)

pkt = header + normal + dns
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.sendto(pkt, (host, port))
print(f"sent NetFlow v5 packet ({len(pkt)} bytes, 2 flows: 1 normal + 1 DNS) to {host}:{port}")
print("expect: flows_decoded_total +2, flows_skipped_total +1 (DNS), 1 row inserted")
PY
