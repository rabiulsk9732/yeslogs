# Batch write-ahead log

`clickhouse.spool_dir` enables the natlog batch write-ahead log (WAL). It is
embedded in natlog; no Filebeat or sidecar service is required.

## State machine

1. A writer flushes a normalized batch to `<sequence>.spool.tmp`.
2. The payload is wrapped with a format marker and SHA-256 checksum, then the
   file is `fsync`ed.
3. Atomic rename creates `<sequence>.spool.active`, followed by a directory
   `fsync`. Only then may the first ClickHouse insert start.
4. A durable ClickHouse acknowledgement removes the active file and `fsync`s
   the directory.
5. Exhausted retries rename the file to `<sequence>.spool`; the replay loop
   consumes ready files oldest-first.

Active files are deliberately invisible to live replay, preventing a race with
their owning writer. On startup, every active file is atomically adopted as a
ready file. This covers process or host failure between database send and local
commit. Incomplete `.tmp` files were never acknowledged as durable and are
discarded.

## Delivery and deduplication

Delivery remains at-least-once. A database commit followed by a lost response
can cause replay. A persistent random identity in `spool_dir/.wal-id` namespaces
the WAL filename and the combined value is supplied to ClickHouse as
`insert_deduplication_token`. This prevents equal sequence numbers on different
collectors from colliding. `Replicated*MergeTree` can suppress a genuine replay
within its configured deduplication window. Plain `MergeTree` cannot.

Async inserts must use `wait_for_async_insert: true` while the WAL is enabled.
Configuration validation rejects fire-and-forget async inserts because natlog
cannot safely know when to delete the WAL file.

## Failure handling

- Unreadable or repeatedly rejected files move to `spool_dir/quarantine`; they
  are retained as evidence and do not block later batches. Their bytes continue
  to count against `spool_max_gb`, and sequence recovery includes quarantine so
  a later file cannot overwrite them.
- `spool_records_lost_total` increases only after both ClickHouse and the WAL
  fail to retain a batch.
- `spool_files`, `spool_bytes`, and `spool_oldest_seconds` expose backlog.
- When ClickHouse is unavailable at process startup, a WAL-enabled manager
  starts in WAL-first mode. Inserts and replay reconnect automatically, so UDP
  listeners can continue accepting traffic.
- The packaged systemd unit orders natlog after ClickHouse and requests that it
  start, but does not require it to remain active. Stopping ClickHouse therefore
  does not intentionally stop the WAL-capable collector.

Do not delete files manually while natlog is running. Copy a quarantined file
before forensic work and preserve its mode/metadata.

## Boundary

This WAL protects every batch from the moment the writer begins flushing it.
Records still sitting in the bounded in-memory writer queue or an unfinished
partial batch have not reached the WAL yet. Eliminating that final crash window
requires a durable ingress broker or a separate per-packet journal. UDP itself
has no sender acknowledgement/retransmission guarantee.
