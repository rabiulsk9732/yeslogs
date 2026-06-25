# Field validation with real-device captures

Validate the decoders against real exporters by capturing their traffic,
**sanitizing** it, and replaying it into a collector — no production database or
live device required for the replay step.

```
capture (tcpdump)  ->  sanitize (pcapsanitize)  ->  replay (pcapreplay)  ->  collector
   real IPs              IPs rewritten to 10.x        safe to share          decode + insert
```

## 0. Privacy first (read this)

Raw captures contain **real exporter and subscriber IPs** in headers *and* flow
records. Always sanitize before sharing/committing:

```bash
pcapsanitize --in real.pcap --out safe.pcap
```

`pcapsanitize` rewrites every IPv4 address deterministically (consistent within a
run) into `10.0.0.0/8`, template-aware for v9/IPFIX, and **drops** any v9/IPFIX
data packet whose template it has not seen (so nothing leaks). It never prints
original addresses. Pass `--key <hex>` for reproducible output.

## 1. Capture (on the collector host or a span port)

```bash
sudo scripts/capture-device.sh <interface> real.pcap 60   # 60 seconds
# ports captured: 2055 (v5), 9995 (v9), 4739 (IPFIX)
```

> Firewall reminder: in production only the known exporter IPs should be able to
> reach these UDP ports (`deploy/ufw-example.sh`, and the device registry with
> `security.unknown_exporter_mode: reject`). Capturing does not change that.

## 2. Sanitize, then replay

```bash
pcapsanitize --in real.pcap --out safe.pcap
scripts/replay-pcap.sh safe.pcap 127.0.0.1:9995 1x netflow9   # or 10x to go faster
```

`--speed 1x` preserves capture timing; `10x` / `max` replay faster. `--loop`
repeats. Watch the collector decode it:

```bash
curl -s 127.0.0.1:9101/metrics | grep -E 'packets_received|templates_received|flows_decoded|flows_inserted|template_unknown'
```

## Per-vendor notes

### MikroTik (NetFlow v9, port 9995)
1. `sudo scripts/capture-device.sh eth0 mtk.pcap 60`
2. `pcapsanitize --in mtk.pcap --out mtk-safe.pcap`
3. `scripts/replay-pcap.sh mtk-safe.pcap 127.0.0.1:9995 1x netflow9`
See `docs/devices/mikrotik.md` for the router-side config.

### Cisco (Flexible NetFlow v9, port 9995; or IPFIX 4739)
Same flow; use `--proto netflow9` (or `ipfix` and target `:4739`). Set a short
`template data timeout` so templates appear early in the capture. See
`docs/devices/cisco.md`.

### Juniper (IPFIX / inline-jflow, port 4739)
`scripts/replay-pcap.sh jnpr-safe.pcap 127.0.0.1:4739 1x ipfix`. IPFIX uses
absolute timestamps and may use variable-length / enterprise fields — all decoded
(enterprise fields safely skipped). See `docs/devices/juniper.md`.

### Huawei / NetStream (v9, port 9995; or IPFIX 4739)
Same as MikroTik/Cisco depending on `version 9` vs `version 10`. Some platforms
send templates infrequently — capture long enough to include a template refresh.
See `docs/devices/huawei.md`.

## Troubleshooting: unknown templates

For v9/IPFIX, data records can only be decoded after their template arrives.

- **`template_unknown_total` rising, `flows_decoded_total` flat** → the capture
  starts mid-stream (templates not yet seen). Capture longer to include a
  template refresh, or `--loop` the replay so the templates (sent periodically)
  are seen before the data on a later pass.
- **`pcapsanitize` reports `dropped` packets** → those data packets referenced a
  template not present in the capture; they were dropped to avoid leaking real
  IPs. Re-capture a window that includes the template flowsets.
- **Nothing received** (`packets_received_total` flat) → wrong target port for
  the protocol, or the payload protocol doesn't match `--proto`. Confirm with
  `tcpdump -ni any udp port 9995` while replaying.

## Checklist

- [ ] Captured with `scripts/capture-device.sh`
- [ ] Sanitized with `pcapsanitize` (0 unexpected drops)
- [ ] Replayed with `scripts/replay-pcap.sh` into a test collector
- [ ] `flows_decoded_total` > 0 and rows present in ClickHouse
- [ ] Only the sanitized pcap is shared/committed
