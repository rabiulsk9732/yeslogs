# natflow-dataplane

A high-performance UDP **dataplane collector** for ISP NetFlow / IPFIX logs.
It receives flow exports, decodes and normalizes them, applies skip rules, and
batch-inserts the survivors into [ClickHouse](https://clickhouse.com/). No
Docker, no Kafka, no PostgreSQL — a single static Go binary under systemd.

```
 NetFlow/IPFIX UDP ─▶ receiver ─▶ decoder ─▶ normalizer ─▶ rules ─▶ batch writer ─▶ ClickHouse
                         │                                                  │
                         └────────────── Prometheus metrics ───────────────┘
```

## Status

| Protocol   | Port (default) | Decoding                                          |
| ---------- | -------------- | ------------------------------------------------- |
| NetFlow v5 | 2055           | **Fully implemented**                             |
| NetFlow v9 | 9995           | **Template decode** (data + options + NAT fields) |
| IPFIX      | 4739           | **Template decode** (varlen + enterprise + NAT)   |

NetFlow v9 and IPFIX are template-based: the decoders cache templates per
(exporter, source-id/observation-domain, template-id), decode data records
against them (including CGNAT post-NAT address/port fields), and skip Options
Template records. IPFIX additionally handles variable-length fields and safely
skips enterprise-specific information elements. Data records that arrive before
their template are counted under `template_unknown_total` until it is learned.

## Layout

```
cmd/collector            entrypoint + wiring + graceful shutdown
internal/config          YAML load, defaults, validation
internal/receiver        UDP listeners + worker pool
internal/decoder         common Flow type + Decoder interface
internal/decoder/netflow5 NetFlow v5 decoder (complete)
internal/decoder/netflow9 NetFlow v9 decoder (template cache + data decode)
internal/decoder/ipfix    IPFIX decoder (templates + varlen + enterprise)
internal/normalizer      decoder.Flow -> canonical FlowRecord
internal/rules           skip filters (DNS / private->private / zero-byte)
internal/pipeline        decode -> normalize -> rules -> enqueue
internal/writer/clickhouse batching, retrying ClickHouse writer
internal/metrics         Prometheus metrics + HTTP server
internal/logger          structured JSON logging (slog)
configs/collector.yaml   configuration
migrations/clickhouse.sql flow_logs schema
systemd/                 service unit
scripts/                 build / install / test-send
```

## Requirements

- Ubuntu 24.04 (or any modern Linux)
- Go 1.23+ (to build)
- ClickHouse server reachable on the native protocol port (default `9000`)
- `python3` (only for `scripts/test-send.sh`)

## Build

```bash
git clone <repo> natflow-dataplane
cd natflow-dataplane
go mod download
./scripts/build.sh          # -> bin/natflow-collector
```

This produces a static binary (`CGO_ENABLED=0`).

## ClickHouse

The collector assumes ClickHouse is already installed and running. Quick install
on Ubuntu:

```bash
curl https://clickhouse.com/ | sh
sudo ./clickhouse install
sudo clickhouse start
```

Apply the schema (creates the `natlogs` database and `flow_logs` table):

```bash
clickhouse-client --multiquery < migrations/clickhouse.sql
```

The `flow_logs` table is a `MergeTree` partitioned by day, ordered for
`(isp_id, device_id, flow_start, src/dst)` lookups, with a 30-day TTL.

## Configure

Edit `configs/collector.yaml`. Defaults are applied for any omitted field and
unknown keys are rejected. Key settings:

- `server.isp_id` / `server.device_id_default` — stamped onto every row.
- `receiver.workers` — reader goroutines per listener (default: CPU count).
- `receiver.udp_read_buffer_mb` — kernel socket buffer; see sysctl note below.
- `clickhouse.batch_size` / `flush_interval_ms` — flush on whichever comes first.
- `clickhouse.queue_capacity` — in-memory backlog before flows are dropped.
- `rules.*` — toggle the skip filters.

### Kernel tuning (recommended for high PPS)

`udp_read_buffer_mb` is capped by `net.core.rmem_max`. Raise it so bursts are not
dropped by the kernel before the collector reads them:

```bash
sudo sysctl -w net.core.rmem_max=134217728   # 128 MiB
# persist:
echo 'net.core.rmem_max=134217728' | sudo tee /etc/sysctl.d/99-natflow.conf
```

If the buffer cannot be set, the collector logs a warning and continues with the
kernel default.

## Run (foreground)

```bash
./bin/natflow-collector --config configs/collector.yaml
```

## Install as a systemd service

```bash
sudo ./scripts/install.sh            # builds, installs to /opt/natflow-dataplane
sudo systemctl enable --now natflow-collector
systemctl status natflow-collector
journalctl -u natflow-collector -f
```

`install.sh` copies the binary, config (only if absent), migration and the unit
file to `/opt/natflow-dataplane` and `/etc/systemd/system`.

## Production operations

Helper scripts live in `scripts/` and `deploy/`:

| File                        | Purpose                                                        |
| --------------------------- | ------------------------------------------------------------- |
| `scripts/benchmark.sh`      | Run the benchgen load ladder (1k→20k pps) against a collector  |
| `scripts/healthcheck.sh`    | Check systemd service + metrics endpoint + today's CH rows     |
| `scripts/backup-daily.sh`   | Write a daily summary snapshot of `flow_logs` (cron-friendly)  |
| `deploy/logrotate/natflow`  | logrotate rules for `/var/log/natflow/*.log`                   |
| `deploy/ufw-example.sh`     | Restrict the UDP ports to a single exporter IP (safe by default) |

### Production checklist

```bash
# 1. install + enable the service (see "Install as a systemd service")
sudo ./scripts/install.sh
sudo systemctl enable --now natflow-collector

# 2. log rotation (prevents the log filling the disk)
sudo install -m 0644 deploy/logrotate/natflow /etc/logrotate.d/natflow
sudo logrotate --debug /etc/logrotate.d/natflow   # verify

# 3. firewall: allow only the exporter device to the UDP ports
DEVICE_IP=<exporter-ip> ./deploy/ufw-example.sh

# 4. health check (exit non-zero on failure -> use in monitoring/cron)
./scripts/healthcheck.sh

# 5. daily summary backup (cron, e.g. 02:00)
#    0 2 * * * /opt/natflow-dataplane/scripts/backup-daily.sh
./scripts/backup-daily.sh

# 6. (optional) capacity test before onboarding a busy device
./scripts/benchmark.sh
```

Keep `logging.level` at `info` (default) in production — decode-failure logs are
emitted only at `debug`, so there is no per-packet log spam by default. Do not
raise to `debug` against a live device.

## UDP ports

| Listener   | Default port | Protocol |
| ---------- | ------------ | -------- |
| NetFlow v5 | 2055         | UDP      |
| NetFlow v9 | 9995         | UDP      |
| IPFIX      | 4739         | UDP      |

Open them on the host firewall, e.g.:

```bash
sudo ufw allow 2055/udp
```

Point your exporter (MikroTik / Cisco / Juniper) at the collector's IP and the
matching port.

## Metrics

Prometheus metrics are served at `http://127.0.0.1:9101/metrics`, with a
`/healthz` liveness probe.

| Metric                       | Meaning                                              |
| ---------------------------- | --------------------------------------------------- |
| `packets_received_total`     | UDP datagrams received (all listeners)              |
| `packets_dropped_total`      | datagrams dropped (malformed / decode errors)      |
| `packets_unsupported_total`  | datagrams for a recognized-but-undecodable protocol (none currently) |
| `flows_decoded_total`        | flow records decoded                                |
| `flows_skipped_total`        | flows dropped by skip rules                         |
| `flows_inserted_total`       | flows inserted into ClickHouse                      |
| `flows_dropped_total`        | flows dropped without insertion (queue full / shutdown deadline) |
| `flows_rejected_total`       | flows rejected by ClickHouse during row append      |
| `insert_errors_total`        | batch inserts that failed after retries             |
| `templates_received_total`   | NetFlow v9/IPFIX templates parsed                   |
| `template_unknown_total`     | data flowsets dropped (template not yet known)      |
| `current_queue_size`         | flows currently buffered in the writer queue        |

> A brief rise in `template_unknown_total` at startup is normal — NetFlow
> v9/IPFIX data that arrives before its template is dropped until the template
> is learned.

## Test without a real exporter

With the collector running and ClickHouse up:

```bash
scripts/test-send.sh 127.0.0.1 2055
```

This sends one datagram with two flows — a normal HTTPS flow and a DNS flow.
Expected result:

- `flows_decoded_total` increases by 2
- `flows_skipped_total` increases by 1 (the DNS flow)
- one row lands in ClickHouse:

```bash
clickhouse-client -q "SELECT src_ip, dst_ip, dst_port, bytes, flow_type FROM natlogs.flow_logs ORDER BY created_at DESC LIMIT 5"
curl -s 127.0.0.1:9101/metrics | grep -E 'flows_(decoded|skipped|inserted)_total'
```

## Benchmarking

`cmd/benchgen` is a NetFlow v5/v9/IPFIX load generator for stress-testing the collector.

```bash
go build -o bin/benchgen ./cmd/benchgen
```

It emits valid datagrams at a target packet rate, with a configurable mix of
DNS / private / zero-byte flows so the skip rules are exercised under load. Each
packet carries `--flows-per-packet` flows (v5/v9 max 30), so flows/sec = pps ×
flows-per-packet. It prints packets/s, flows/s, Mbps and send_errors every second.
With `--proto netflow9` (port 9995) or `--proto ipfix` (port 4739) it injects the
template first and refreshes it periodically, then sends template-described data
packets.

Flags: `--target`, `--proto` (netflow5|netflow9|ipfix), `--pps`, `--flows-per-packet`,
`--duration`, `--dns-percent`, `--private-percent`, `--zero-byte-percent`,
`--senders` (concurrent sockets; raise for high pps), `--ring-size`.

Ramp the rate (collector + ClickHouse must be running):

```bash
bin/benchgen --target 127.0.0.1:2055 --pps 1000  --flows-per-packet 30 --duration 60s --dns-percent 30 --private-percent 20 --zero-byte-percent 5
bin/benchgen --target 127.0.0.1:2055 --pps 5000  --flows-per-packet 30 --duration 60s --dns-percent 30 --private-percent 20 --zero-byte-percent 5
bin/benchgen --target 127.0.0.1:2055 --pps 10000 --senders 2 --flows-per-packet 30 --duration 60s --dns-percent 30 --private-percent 20 --zero-byte-percent 5
bin/benchgen --target 127.0.0.1:2055 --pps 50000 --senders 3 --flows-per-packet 30 --duration 60s --dns-percent 30 --private-percent 20 --zero-byte-percent 5
```

Watch the collector side in another terminal:

```bash
watch -n1 "curl -s 127.0.0.1:9101/metrics | grep -E '^(packets_received|flows_decoded|flows_inserted|flows_dropped|current_queue_size)'"
```

How to read it:

- **`packets_received` < packets sent** → kernel UDP loss; raise
  `receiver.udp_read_buffer_mb` and `net.core.rmem_max`, or add workers.
- **`current_queue_size` climbing + `flows_dropped_total` rising** → ClickHouse
  insert can't keep up; scale ClickHouse (cores/nodes, async inserts) or raise
  `batch_size`. The queue caps and sheds load rather than crashing.
- **`insert_errors_total` > 0** → ClickHouse erroring/rejecting batches.

### Reference results

Single **6-core VPS** with generator + collector + ClickHouse all on the same
box (worst case — they compete for CPU), 30 flows/packet, mix 30% DNS / 20%
private / 5% zero-byte (≈55% skipped), 10s per rung, queue cap 1,000,000:

| offered pps | flows/s in | UDP loss | decoded/s | inserted/s | queue peak | dropped |
| ----------- | ---------- | -------- | --------- | ---------- | ---------- | ------- |
| 10,000      | 300k       | 0.0%     | 300k      | 134k       | 12k        | 0       |
| 15,000      | 450k       | 0.0%     | 450k      | 201k       | 26k        | 0       |
| 20,000      | 600k       | 0.0%     | 599k      | 257k       | 591k       | 0       |
| 25,000      | 750k       | 0.0%     | 750k      | 263k       | full       | 558k    |
| 50,000      | 1.5M       | 0.0%     | 1.5M      | 297k       | full       | 3.9M    |
| 100,000     | 3.0M       | 0.0%     | 3.0M      | 291k       | full       | 10.5M   |

Takeaways: the ingest path (receive → decode → normalize → rules) sustained
**3M flows/s with 0% UDP loss** even on a shared box. The bottleneck is
**ClickHouse insert throughput (~260–300k flows/s here)**; beyond it the writer
queue fills and sheds load (no errors, no crash). Clean sustained sweet spot on
this box: **≤20k pps**. To go higher, give ClickHouse more resources — the Go
dataplane has large headroom.

## Delivery semantics

Inserts are **at-least-once**. The writer batches flows and retries transient
ClickHouse failures; if a batch's acknowledgement is lost *after* the server
already committed it (e.g. a timeout while reading the response), the retry can
create duplicate rows. The default `MergeTree` does not deduplicate, so exact
byte/packet/flow counts are not guaranteed under failure.

To get idempotent inserts, run a `Replicated*MergeTree` (native insert-block
deduplication) or use `ReplacingMergeTree` keyed on a stable row identity and
query with `FINAL`. See `migrations/clickhouse.sql`.

On shutdown the writer drains the queue into ClickHouse for up to
`clickhouse.shutdown_drain_ms` (default 15s); anything still buffered after that
is dropped and counted under `flows_dropped_total` so the process always exits
promptly (the systemd unit sets `TimeoutStopSec=30` to match). A single row that
ClickHouse rejects on append does not discard its batch — the batch is salvaged
row-by-row and only the bad rows are dropped (`flows_rejected_total`).

## Skip rules

Applied per flow, in this order (first match wins):

1. **zero-byte** — `bytes == 0`
2. **DNS** — source or destination port is 53
3. **private→private** — both endpoints are non-routable (RFC1918, CGNAT
   `100.64/10`, loopback, link-local)

Each is independently toggled in `configs/collector.yaml`.

## Development

```bash
make test     # unit tests (decoder, rules, normalizer)
make vet
make build
```

## Roadmap

- ClickHouse insert tuning (async inserts / multi-writer) to lift the ~300k
  flows/s insert ceiling
- IPv6 flow storage (schema currently IPv4-only)
- Per-exporter device identification
- Director/UI control plane

Done: NetFlow v5, NetFlow v9 (template decode), IPFIX (template + variable-length
+ enterprise + NAT fields), production ops tooling, load generator.
