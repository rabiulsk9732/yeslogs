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
    created_at DateTime DEFAULT now(),

    -- Data-skipping indexes for the reverse-NAT / subscriber lookups. These IPs
    -- are NOT in the primary key, so without them a time-unbounded IP search must
    -- scan the tenant's history; the bloom filter lets ClickHouse skip 32k-row
    -- blocks that cannot contain the IP, keeping point lookups fast at TB scale.
    INDEX idx_nat_ip nat_public_ip TYPE bloom_filter(0.01) GRANULARITY 4,
    INDEX idx_src_ip src_ip TYPE bloom_filter(0.01) GRANULARITY 4
)
ENGINE = MergeTree
PARTITION BY event_date
ORDER BY (isp_id, device_id, flow_start, src_ip, src_port, dst_ip, dst_port)
TTL event_date + INTERVAL 180 DAY
SETTINGS index_granularity = 8192;

-- Dashboard rollups (also auto-created + maintained by natlog on startup). natlog
-- runs a PERIODIC BATCH rollup (every ~2 min) into these summary tables — NOT a
-- per-insert materialized view, so rollup part-creation stays independent of
-- ingest rate and scales to hundreds of exporters. The console reads a few
-- hundred rows instead of scanning raw flow_logs → O(1) dashboard at any size.
CREATE TABLE IF NOT EXISTS natlogs.flow_rollup
(
    isp_id UInt32, event_date Date, hour UInt8, device_id UInt32,
    flows SimpleAggregateFunction(sum, UInt64),
    bytes SimpleAggregateFunction(sum, UInt64),
    packets SimpleAggregateFunction(sum, UInt64),
    tcp SimpleAggregateFunction(sum, UInt64),
    udp SimpleAggregateFunction(sum, UInt64),
    icmp SimpleAggregateFunction(sum, UInt64),
    other SimpleAggregateFunction(sum, UInt64),
    dur_sum SimpleAggregateFunction(sum, UInt64),
    subs AggregateFunction(uniq, IPv4),
    nat_ips AggregateFunction(uniq, IPv4)
) ENGINE = AggregatingMergeTree PARTITION BY event_date ORDER BY (isp_id, event_date, hour, device_id);

CREATE TABLE IF NOT EXISTS natlogs.flow_rollup_subs
(
    isp_id UInt32, event_date Date, src_ip IPv4,
    bytes SimpleAggregateFunction(sum, UInt64)
) ENGINE = SummingMergeTree PARTITION BY event_date ORDER BY (isp_id, event_date, src_ip);

-- Watermark: max flow_logs.created_at rolled up so far (batch job continues here).
CREATE TABLE IF NOT EXISTS natlogs.flow_rollup_state
(ts DateTime, at DateTime DEFAULT now()) ENGINE = MergeTree ORDER BY ts TTL at + INTERVAL 2 DAY;
