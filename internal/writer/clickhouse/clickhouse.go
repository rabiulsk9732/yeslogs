// Package clickhouse implements a batching, retrying writer that inserts
// FlowRecords into the ClickHouse flow_logs table. Rows are always written in
// batches on the hot path; a single rejected row triggers a best-effort
// row-by-row salvage so it cannot discard the whole batch.
//
// Delivery is at-least-once: a batch whose acknowledgement is lost (e.g. a
// timeout while reading the server response after the data was already
// committed) is retried and may produce duplicate rows. See migrations and the
// README "Delivery semantics" section for how to make inserts idempotent.
package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

const insertStmt = `INSERT INTO %s.flow_logs
	(isp_id, device_id, src_ip, src_port, dst_ip, dst_port,
	 nat_public_ip, nat_public_port, protocol, bytes, packets,
	 flow_start, flow_end, flow_type, exporter_ip)`

const (
	maxAttempts            = 3
	sendTimeout            = 30 * time.Second
	defaultShutdownTimeout = 15 * time.Second
)

// Config configures the writer.
type Config struct {
	Addr            string
	Database        string
	Username        string
	Password        string
	BatchSize       int
	FlushInterval   time.Duration
	QueueCapacity   int
	ShutdownTimeout time.Duration // max time to drain the queue on Stop
}

// appendError marks a failure that occurred while appending a row to a batch.
// Such errors are deterministic (bad/incompatible data), so they are not worth
// retrying; the batch is salvaged row-by-row instead.
type appendError struct{ err error }

func (e *appendError) Error() string { return e.err.Error() }
func (e *appendError) Unwrap() error { return e.err }

// Writer buffers FlowRecords and flushes them to ClickHouse in batches.
type Writer struct {
	cfg     Config
	conn    driver.Conn
	queue   chan normalizer.FlowRecord
	metrics *metrics.Metrics
	log     *slog.Logger
	insert  string

	wg   sync.WaitGroup
	done chan struct{}
}

// New connects to ClickHouse (verifying connectivity with a ping) and returns a
// ready writer. Call Start to begin the flush loop.
func New(cfg Config, m *metrics.Metrics, log *slog.Logger) (*Writer, error) {
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		Compression:  &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout:  5 * time.Second,
		MaxOpenConns: 4,
		MaxIdleConns: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse %s: %w", cfg.Addr, err)
	}
	return &Writer{
		cfg:     cfg,
		conn:    conn,
		queue:   make(chan normalizer.FlowRecord, cfg.QueueCapacity),
		metrics: m,
		log:     log.With("component", "clickhouse"),
		insert:  fmt.Sprintf(insertStmt, cfg.Database),
		done:    make(chan struct{}),
	}, nil
}

// Enqueue submits rec for insertion without ever blocking. If the queue is full
// it returns false and the caller should count a drop.
func (w *Writer) Enqueue(rec normalizer.FlowRecord) bool {
	select {
	case w.queue <- rec:
		w.metrics.QueueSize.Set(float64(len(w.queue)))
		return true
	default:
		return false
	}
}

// QueueLen returns the number of records currently buffered.
func (w *Writer) QueueLen() int { return len(w.queue) }

// Start launches the background flush loop and returns immediately.
func (w *Writer) Start() {
	w.wg.Add(1)
	go w.loop()
}

func (w *Writer) loop() {
	defer w.wg.Done()
	ticker := time.NewTicker(w.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]normalizer.FlowRecord, 0, w.cfg.BatchSize)
	for {
		select {
		case rec := <-w.queue:
			batch = append(batch, rec)
			w.metrics.QueueSize.Set(float64(len(w.queue)))
			if len(batch) >= w.cfg.BatchSize {
				w.flush(context.Background(), batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(context.Background(), batch)
				batch = batch[:0]
			}
		case <-w.done:
			w.drain(batch)
			return
		}
	}
}

// drain flushes the in-flight batch and everything still queued, bounded by the
// configured shutdown timeout. Anything still buffered when the deadline passes
// is dropped (and counted) so process exit cannot hang on a dead backend.
func (w *Writer) drain(batch []normalizer.FlowRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), w.cfg.ShutdownTimeout)
	defer cancel()

	for {
		if ctx.Err() != nil {
			if lost := len(batch) + len(w.queue); lost > 0 {
				w.metrics.FlowsDropped.Add(float64(lost))
				w.log.Error("shutdown drain deadline exceeded; dropping buffered records",
					"dropped", lost, "timeout", w.cfg.ShutdownTimeout)
			}
			w.metrics.QueueSize.Set(0)
			return
		}
		select {
		case rec := <-w.queue:
			batch = append(batch, rec)
			if len(batch) >= w.cfg.BatchSize {
				w.flush(ctx, batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				w.flush(ctx, batch)
			}
			w.metrics.QueueSize.Set(0)
			return
		}
	}
}

// flush inserts batch, retrying transient errors until ctx is done or attempts
// are exhausted. A deterministic append error is not retried; instead the batch
// is salvaged row-by-row so one bad row cannot lose the rest.
func (w *Writer) flush(ctx context.Context, batch []normalizer.FlowRecord) {
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err = w.sendBatch(ctx, batch); err == nil {
			w.metrics.FlowsInserted.Add(float64(len(batch)))
			w.metrics.QueueSize.Set(float64(len(w.queue)))
			return
		}
		var ae *appendError
		if errors.As(err, &ae) {
			w.salvage(ctx, batch) // deterministic: don't waste retries
			return
		}
		w.log.Warn("batch insert failed", "attempt", attempt, "rows", len(batch), "error", err)
		if attempt < maxAttempts {
			select {
			case <-time.After(backoff(attempt)):
			case <-ctx.Done():
				w.metrics.InsertErrors.Inc()
				w.metrics.FlowsDropped.Add(float64(len(batch)))
				w.log.Error("aborting insert retries due to shutdown deadline", "rows", len(batch))
				return
			}
		}
	}
	w.metrics.InsertErrors.Inc()
	w.metrics.FlowsDropped.Add(float64(len(batch)))
	w.log.Error("dropping batch after exhausting retries", "rows", len(batch), "error", err)
}

// salvage re-inserts a rejected batch one row at a time, keeping the good rows
// and dropping (counting) only the rows ClickHouse rejects. This is the slow
// error-recovery path, not the hot path.
func (w *Writer) salvage(ctx context.Context, batch []normalizer.FlowRecord) {
	w.log.Warn("batch rejected on append; salvaging row-by-row", "rows", len(batch))
	kept, dropped := 0, 0
	for i := range batch {
		if err := w.sendBatch(ctx, batch[i:i+1]); err != nil {
			dropped++
			w.log.Debug("dropping rejected row", "error", err)
			continue
		}
		kept++
	}
	if kept > 0 {
		w.metrics.FlowsInserted.Add(float64(kept))
	}
	if dropped > 0 {
		w.metrics.FlowsRejected.Add(float64(dropped))
		w.log.Error("salvage complete", "kept", kept, "rejected", dropped)
	}
	w.metrics.QueueSize.Set(float64(len(w.queue)))
}

func (w *Writer) sendBatch(ctx context.Context, batch []normalizer.FlowRecord) error {
	cctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	b, err := w.conn.PrepareBatch(cctx, w.insert)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err) // transient: retryable
	}
	for i := range batch {
		r := &batch[i]
		if err := b.Append(
			r.ISPID, r.DeviceID,
			ip4(r.SrcIP), r.SrcPort, ip4(r.DstIP), r.DstPort,
			ip4(r.NatPublicIP), r.NatPublicPort,
			r.Protocol, r.Bytes, r.Packets,
			r.FlowStart, r.FlowEnd, r.FlowType, ip4(r.ExporterIP),
		); err != nil {
			_ = b.Abort()
			return &appendError{fmt.Errorf("append row %d: %w", i, err)}
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err) // transient: retryable
	}
	return nil
}

// Stop signals the flush loop to drain and stop (bounded by ShutdownTimeout),
// waits for it, then closes the ClickHouse connection.
func (w *Writer) Stop() {
	close(w.done)
	w.wg.Wait()
	_ = w.conn.Close()
}

func backoff(attempt int) time.Duration {
	d := time.Duration(attempt) * 250 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// ip4 returns a 4-byte IPv4 address, substituting 0.0.0.0 for nil or non-IPv4
// values so the IPv4 ClickHouse columns always receive a valid value.
func ip4(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return net.IPv4zero.To4()
}
