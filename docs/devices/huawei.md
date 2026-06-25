# Huawei (NetStream) onboarding

Huawei exports flows via **NetStream**, typically **NetFlow v9** format (v9) and
IPFIX on newer NE/CX platforms.

## Router config (NetStream, v9)

```
ip netstream export version 9
ip netstream export source <ROUTER_SOURCE_IP>
ip netstream export host <COLLECTOR_IP> 9995
ip netstream timeout active 1
ip netstream timeout inactive 15

interface GigabitEthernet0/0/1
 ip netstream inbound
 ip netstream outbound
```

For IPFIX-capable platforms use `ip netstream export version 10` and host port 4739.

## natflow device registry entry

```yaml
devices:
  - name: "agg-huawei-01"
    enabled: true
    exporter_ip: "<ROUTER_SOURCE_IP>"   # matches netstream export source
    isp_id: 1
    device_id: 401
    protocol: netflow9          # or ipfix
    profile: huawei
    allowed_ports: [9995]       # or [4739]
    rules:
      skip_dns: true
      skip_private_to_private: true
      skip_zero_bytes: true
```

## Verify

```bash
curl -s 127.0.0.1:9101/metrics | grep -E 'templates_received_total|device_matched_packets_total'
clickhouse-client -q "SELECT count() FROM natlogs.flow_logs WHERE device_id=401"
```

## Notes

- `ip netstream export source` fixes the exporter source IP — use it for
  `exporter_ip`.
- Some Huawei platforms send templates less frequently; if `template_unknown_total`
  stays high, shorten the template refresh / active timeout.
- Confirm v9 vs IPFIX (`version 9` vs `version 10`) and match `protocol`/`allowed_ports`.
