# Juniper (Junos) onboarding

Junos exports flows via **inline-jflow** as **IPFIX** (recommended) or NetFlow v9.

## Router config (inline-jflow, IPFIX)

```
set services flow-monitoring version-ipfix template NATFLOW flow-active-timeout 60
set services flow-monitoring version-ipfix template NATFLOW ipv4-template

set forwarding-options sampling instance NATFLOW input rate 1
set forwarding-options sampling instance NATFLOW family inet output flow-server <COLLECTOR_IP> port 4739
set forwarding-options sampling instance NATFLOW family inet output flow-server <COLLECTOR_IP> version-ipfix template NATFLOW
set forwarding-options sampling instance NATFLOW family inet output inline-jflow source-address <ROUTER_SOURCE_IP>

set chassis fpc 0 sampling-instance NATFLOW
```

## natflow device registry entry

```yaml
devices:
  - name: "edge-juniper-01"
    enabled: true
    exporter_ip: "<ROUTER_SOURCE_IP>"   # matches inline-jflow source-address
    isp_id: 1
    device_id: 301
    protocol: ipfix
    profile: juniper
    allowed_ports: [4739]
    rules:
      skip_dns: true
      skip_private_to_private: true
      skip_zero_bytes: true
```

## Verify

```bash
curl -s 127.0.0.1:9101/metrics | grep -E 'templates_received_total|device_matched_packets_total'
clickhouse-client -q "SELECT count() FROM natlogs.flow_logs WHERE device_id=301 AND flow_type='ipfix'"
```

## Notes

- `inline-jflow source-address` sets the exporter source IP — use it for the
  registry `exporter_ip`.
- IPFIX uses absolute timestamps; natflow decodes flowStart/EndMilliseconds and
  flowStart/EndSeconds (and falls back to export time if absent).
- Sampling `rate 1` = unsampled; if you sample (rate > 1), byte/packet counts
  represent sampled values — account for this in analytics.
