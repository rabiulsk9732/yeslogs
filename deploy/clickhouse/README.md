# ClickHouse tuning for natflow-dataplane

## Server profile

Install the server overlay and restart:

```bash
sudo install -m 0644 deploy/clickhouse/natflow-tuning.xml /etc/clickhouse-server/config.d/natflow-tuning.xml
sudo systemctl restart clickhouse-server
```

## Collector-side levers (collector.yaml, hot-reloadable unless noted)

```yaml
clickhouse:
  writer_workers: 8          # sharded writers; scale toward CH cores
  batch_size: 50000          # rows per native batch (bigger = fewer, larger parts)
  flush_interval_ms: 1000
  max_queue_rows: 200000     # per-writer buffer before backpressure
  compression: "lz4"         # lz4 | lz4hc | zstd | none  (RESTART to change)
  max_open_conns: 0          # 0 = auto (writer_workers*2+2); RESTART to change
  async_insert: false        # ClickHouse server-side async inserts (per-INSERT setting)
  wait_for_async_insert: true  # true = durable ack; false = fire-and-forget (higher tput, weaker durability)
  async_insert_busy_timeout_ms: 1000
```

### Guidance

- **Throughput is dominated by ClickHouse merge/CPU**, not client batching — the
  native batch path already sends large blocks. The biggest wins are giving
  ClickHouse its own cores/disk and increasing `writer_workers` + `batch_size`.
- **`compression: none`** trades network for CPU; on a loopback/LAN colocation it
  can reduce client CPU. On a remote ClickHouse keep `lz4`.
- **`async_insert: true`** lets ClickHouse buffer and merge inserts server-side.
  With `wait_for_async_insert: true` you still get an ack (durable); `false` is
  fire-and-forget (fastest, but a crash can lose the in-flight async buffer).
  Most useful when many collectors/clients insert concurrently.
- Watch `writer_insert_latency_ms`, `writer_queue_size{worker}` and
  `flows_dropped_total` (the Grafana dashboard in `deploy/grafana/`) to find the
  ceiling, then scale workers/ClickHouse accordingly.

Observe async inserts (when enabled):

```sql
SELECT status, count(), avg(bytes) FROM system.asynchronous_insert_log
WHERE event_time > now() - INTERVAL 5 MINUTE GROUP BY status;
```
