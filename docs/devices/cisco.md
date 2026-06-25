# Cisco (IOS / IOS-XE) onboarding

Cisco exports via **Flexible NetFlow** (NetFlow v9) or IPFIX on newer platforms.

## Router config (Flexible NetFlow, v9)

```
flow exporter NATFLOW
 destination <COLLECTOR_IP>
 transport udp 9995
 export-protocol netflow-v9
 template data timeout 60

flow monitor NATFLOW-MON
 exporter NATFLOW
 record netflow ipv4 original-input

interface GigabitEthernet0/0/0
 ip flow monitor NATFLOW-MON input
 ip flow monitor NATFLOW-MON output
```

For IPFIX-capable platforms, use `export-protocol ipfix` and target UDP 4739.

## natflow device registry entry

```yaml
devices:
  - name: "core-cisco-01"
    enabled: true
    exporter_ip: "<ROUTER_SOURCE_IP>"
    isp_id: 1
    device_id: 201
    protocol: netflow9          # or ipfix
    profile: cisco
    allowed_ports: [9995]       # or [4739] for IPFIX
    rules:
      skip_dns: true
      skip_private_to_private: true
      skip_zero_bytes: true
```

## Verify

```bash
curl -s 127.0.0.1:9101/metrics | grep -E 'templates_received_total|device_matched_packets_total'
clickhouse-client -q "SELECT count() FROM natlogs.flow_logs WHERE device_id=201"
```

## Notes

- Set the exporter source interface explicitly (`flow exporter ... source <intf>`)
  so `exporter_ip` is stable and matches the registry entry.
- Cisco templates refresh on the `template data timeout`; keep it modest
  (30–60s) so a restarted collector re-learns templates quickly.
- Match the registry `protocol`/`allowed_ports` to the chosen export protocol.
