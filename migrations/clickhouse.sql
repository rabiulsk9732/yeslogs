-- NATFlow Dataplane ClickHouse schema.
-- Apply with: clickhouse-client --multiquery < migrations/clickhouse.sql
--
-- Delivery is at-least-once: a batch whose acknowledgement is lost after the
-- server already committed it is retried and can create duplicate rows. The
-- plain MergeTree below does NOT deduplicate. If exact counts matter, run a
-- Replicated*MergeTree cluster (native insert-block dedup) or switch to
-- ReplacingMergeTree keyed on a stable row identity and query with FINAL.

CREATE DATABASE IF NOT EXISTS natlogs;

CREATE TABLE IF NOT EXISTS natlogs.flow_logs
(
    event_date Date DEFAULT toDate(flow_start),
    isp_id UInt32,
    device_id UInt32,

    src_ip IPv4,
    src_port UInt16,
    dst_ip IPv4,
    dst_port UInt16,

    nat_public_ip IPv4 DEFAULT toIPv4('0.0.0.0'),
    nat_public_port UInt16 DEFAULT 0,

    protocol UInt8,
    bytes UInt64,
    packets UInt64,

    flow_start DateTime,
    flow_end DateTime,

    flow_type LowCardinality(String),
    exporter_ip IPv4,
    created_at DateTime DEFAULT now()
)
ENGINE = MergeTree
PARTITION BY event_date
ORDER BY (isp_id, device_id, flow_start, src_ip, src_port, dst_ip, dst_port)
TTL event_date + INTERVAL 180 DAY
SETTINGS index_granularity = 8192;
