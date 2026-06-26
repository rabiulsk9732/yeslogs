# Scaling YesLogs Director — to 1000s of TB and millions of flows/sec

This is the horizontal-scale playbook. Nothing here is a hard wall: the platform
is built so each tier scales out independently. Numbers below are anchored to a
measured baseline; multiply by node count as you grow.

## Measured baseline (one shared 6-vCPU box, generator co-resident)

- **Sustained ingest ~218k flows/s, 0 drops** (12 writer workers, 50k batch).
- **Bottleneck = the ClickHouse writer**, never the UDP receiver/decoder
  (decoded 11M+ with 0 drops under blocking backpressure). `async_insert` is on.
- **On-disk ~1.5 B/flow synthetic (39× compression); ~10–15 B/flow realistic.**

The 218k number is *this tiny box's* floor. ClickHouse clusters do millions of
inserts/sec; the lever is sharding + dedicated cores, not a code limit.

## The tiers and how each scales

```
exporters ──udp──> [collector(s)] ──> [ingest buffer] ──> [ClickHouse cluster] ──lifecycle──> [S3 cold (Zata)]
  MikroTik/Cisco      stateless         in-proc queue        sharded+replicated      Parquet, Hive-partitioned
   NetFlow/IPFIX      (N instances)     (or Kafka, see §3)   (N shards × R replicas)  cold-search via s3()
```

### 1. Ingest throughput (single node)
Already in place and live-tunable in **Settings → Dataplane**:
- **`async_insert=1`** (+ `wait_for_async_insert`, `async_insert_busy_timeout_ms`) — server-side batching.
- **Large batches** are the #1 lever (56k→218k just by batch size). `batchSize` 25k–100k.
- **Bounded in-process queue** (`maxQueueRows`) absorbs bursts; **`backpressureMode=block`** = zero drops (receiver auto-throttles to writer drain rate).
- **Writer workers** scale with cores (sweet spot ≈ cores; too many regresses on contention).

### 2. Horizontal: ClickHouse cluster (the big multiplier)
For >single-node ingest or >hot-storage capacity:
- **Shard** `flow_logs` across N nodes (shard key `cityHash64(isp_id, device_id)` so a tenant's data co-locates). Near-linear: 4 shards ≈ 4× ingest + 4× hot capacity.
- **ReplicatedMergeTree + R replicas per shard** (needs ClickHouse Keeper) for HA + read scaling.
- A **Distributed** table fans inserts/queries across shards. The collector writes to the Distributed table (or directly to a shard for locality).
- This is the **"multi-dataplane per capec"** path: many stateless collectors + a CH cluster, all driven by the one Director registry.

### 3. Durable ingest buffer (Kafka / Redis Streams / RabbitMQ / NATS)
The in-process queue is fast but **in-memory** (lost on restart, no replay). At ISP scale add a **durable** buffer between collector and ClickHouse:
- **Why:** decouple ingest from insert, absorb multi-minute bursts, **zero-drop**, **replay** after a ClickHouse outage, multiple independent consumers (store + real-time alerting).
- **Kafka** is the standard for NetFlow-at-scale (goflow2 → Kafka → ClickHouse). ClickHouse has a **native Kafka table engine** that consumes straight into MergeTree — collectors just produce, no custom consumer.
- **Redis Streams / NATS JetStream** are lighter-weight alternatives for mid-scale; **RabbitMQ** if you already run it.
- **Where it fits:** collector decodes/normalizes → produces to topic `flows.<isp>` → CH Kafka engine (or a batch consumer) inserts. The Director/registry is unchanged.
- **When to add:** when one node's ingest saturates, when you need restart-safe/replayable ingestion, or when a second consumer (alerting/ML) appears. Not before — it's real operational weight.

### 4. Storage for 1000s of TB (hot → cold tiering)
- **Hot (NVMe, ClickHouse):** keep the working window (`archiveAfterDays`, default 7d). Bounded by design.
- **Cold (S3 / Zata, Parquet):** the auto-archival sweep streams each aged day **directly from ClickHouse to S3 in Parquet** (`INSERT INTO FUNCTION s3()`), then `DROP PARTITION`. Hive layout `isp_id=/year=/month=/day=/part-000.parquet`. Columnar + compressed → cheap at PB scale.
- **Alternative/complement: ClickHouse tiered storage** (`storage_policy` with an `s3` disk + `move_factor`) lets CH transparently move old *parts* to S3 while staying queryable as one table — no app-level archival. Trade-off: less control over format/layout vs. the explicit Parquet archive (which is portable + queryable by any S3/Athena/DuckDB tool). Both can coexist.
- **Retention TTL** (`event_date + N days`) is the final backstop.

### 5. Cold search at scale (lawful 3–6 month queries)
- **Parquet + predicate pushdown:** `s3(..., 'Parquet')` reads only needed columns/row-groups.
- **Date-path pruning (implemented):** the search reads *only* the exact archived days overlapping `[from,to]` (`isp_id=N/{year=…/day=…}` brace glob), never the whole bucket. Recent-only searches skip S3 entirely.
- **`s3Cluster()`** distributes the S3 read across all cluster nodes for parallelism on wide ranges.
- **Graceful:** S3 slow/down → hot results still returned; search never hard-fails.

## Capacity math (rule of thumb)
- Subscribers ≈ `sustained_flows_per_sec ÷ flows_per_sec_per_subscriber`. Per node @218k/s: ~436k subs @0.5 f/s. Per **cluster**, multiply by shard count.
- Hot storage/day ≈ `flows_per_day × ~12 B`. Example 100k subs @0.5 f/s ≈ 52 GB/day hot → 7-day window ≈ 363 GB hot; the rest streams to Zata.

## Phased roadmap (no rewrites — additive)
1. **Now:** single unified node; async_insert + big batches; auto-archive to Zata (Parquet); cold search with date pruning. ✅
2. **Scale-out reads/HA:** add ClickHouse replica(s) + Keeper; `s3Cluster()` for cold reads.
3. **Scale-out ingest:** shard ClickHouse (Distributed table); run multiple stateless collectors behind the same Director registry.
4. **Durable ingest:** introduce Kafka (CH Kafka engine) when ingest saturates or replay/HA is required.
5. **PB-scale storage:** ClickHouse tiered `s3` storage_policy alongside the Parquet archive; lifecycle rules on the bucket.

Each step is independent — adopt only what the load demands, in this order.
