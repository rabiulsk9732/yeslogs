# Generic IPFIX / NetFlow exporter onboarding

Use this for any exporter not covered by a vendor-specific guide (nProbe,
softflowd, VPP, pmacct, other routers). natflow decodes standard NetFlow v5,
NetFlow v9, and IPFIX (RFC 7011) — no vendor lock-in.

## What the exporter must do

- Send to the collector on the matching UDP port:
  - NetFlow v5 → `2055`
  - NetFlow v9 → `9995`
  - IPFIX (v10) → `4739`
- For v9/IPFIX, send templates periodically (the collector decodes data records
  only after it has learned the template).
- Use a **stable source IP** for the export packets and register that IP.

## Minimum useful fields

natflow maps these information elements (others are safely ignored):

| Field | NetFlow v9 / IPFIX IE |
| ----- | --------------------- |
| src/dst IPv4 | 8 / 12 |
| src/dst port | 7 / 11 |
| protocol | 4 |
| bytes / packets | 1 / 2 |
| flow start/end | 22/21 (v9 uptime) or 150/151 (sec), 152/153 (ms) (IPFIX) |
| NAT (CGNAT) post-NAT src ip/port | 225 / 227 |

## natflow device registry entry

```yaml
devices:
  - name: "exporter-generic-01"
    enabled: true
    exporter_ip: "<EXPORTER_SOURCE_IP>"
    isp_id: 1
    device_id: 901
    protocol: auto          # netflow5 | netflow9 | ipfix | auto
    profile: generic
    allowed_ports: [2055, 9995, 4739]
    rules:
      skip_dns: true
      skip_private_to_private: true
      skip_zero_bytes: true
```

## Verify

```bash
curl -s 127.0.0.1:9101/metrics | grep -E 'packets_received_total|templates_received_total|device_matched_packets_total|unknown_exporter_packets_total'
clickhouse-client -q "SELECT flow_type, count() FROM natlogs.flow_logs WHERE device_id=901 GROUP BY flow_type"
```

## Troubleshooting

- `unknown_exporter_packets_total` rising but `device_matched_packets_total` flat
  → the exporter's source IP does not match `exporter_ip`. Check the actual
  source with `tcpdump -ni any udp port 9995` and update the registry.
- `template_unknown_total` stays high → templates not arriving; shorten the
  exporter's template refresh interval.
- IPFIX variable-length and enterprise fields are decoded/skipped safely.
