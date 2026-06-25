# MikroTik (RouterOS) onboarding

MikroTik exports **NetFlow v9** (also called "Traffic Flow"). v5 is supported on
older RouterOS but v9 is recommended (carries more fields).

## Router config (RouterOS)

```
/ip traffic-flow
set enabled=yes interfaces=all cache-entries=4k active-flow-timeout=1m inactive-flow-timeout=15s

/ip traffic-flow target
add dst-address=<COLLECTOR_IP> port=9995 version=9
```

CGNAT/NAT logging (if you run CGNAT and want post-NAT addresses), enable the
relevant IPFIX/NetFlow NAT fields if your RouterOS version supports them.

## natflow device registry entry

```yaml
devices:
  - name: "rtr-mikrotik-01"
    enabled: true
    exporter_ip: "<ROUTER_SOURCE_IP>"   # the IP the router sends FROM
    isp_id: 1
    device_id: 101
    protocol: netflow9
    profile: mikrotik
    allowed_ports: [9995]
    rules:
      skip_dns: true
      skip_private_to_private: true
      skip_zero_bytes: true
```

Set `security.unknown_exporter_mode: "reject"` once the device is listed.

## Verify

```bash
# templates should be learned within ~1 template interval, then flows decode:
curl -s 127.0.0.1:9101/metrics | grep -E 'templates_received_total|device_matched_packets_total|flows_inserted_total'

clickhouse-client -q "SELECT count(), max(flow_start) FROM natlogs.flow_logs WHERE device_id=101"
```

## Notes

- The `exporter_ip` must match the router's **source** IP for the flow packets
  (often the loopback/router-id, not necessarily the egress interface). Check
  `unknown_exporter_packets_total` rising if it doesn't match.
- RouterOS sends v9 templates periodically; a brief `template_unknown_total`
  rise at startup is normal until the first template arrives.
- Firewall: allow only this source IP to UDP 9995 (see README firewall reminder).
