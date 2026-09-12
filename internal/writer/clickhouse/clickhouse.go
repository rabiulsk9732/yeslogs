// Package clickhouse implements a sharded, batching, retrying writer pool that
// inserts FlowRecords into the ClickHouse flow_logs table. Records are sharded
// by (exporter_ip, device_id) across N independent writers, each building its
// own batches; rows are always written in batches, never one-by-one.
//
// The pool is hot-swappable: a config reload that changes the worker count or
// queue capacity builds a fresh pool, atomically swaps it in, and drains the old
// one in the background. Per-worker queues shed load according to the configured
// backpressure mode so the collector never blocks fatally or crashes under load.
//
// Delivery is at-least-once (see migrations and the README "Delivery semantics").
package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/writer/spool"
)

const insertStmt = `INSERT INTO %s.flow_logs
	(isp_id, device_id, src_ip, src_port, dst_ip, dst_port,
	 nat_public_ip, nat_public_port, nat_dest_ip, nat_dest_port, nat_event, username, protocol, bytes, packets,
	 flow_start, flow_end, exporter_flow_start, exporter_flow_end, collector_received, flow_type, exporter_ip,
	 src_ip_v6, dst_ip_v6, nat_public_ip_v6, nat_dest_ip_v6, exporter_ip_v6)`

// schemaDDL brings an existing flow_logs up to the columns this build writes.
// Idempotent and metadata-only in ClickHouse — ADD COLUMN with a DEFAULT does
// not rewrite existing parts, so it is safe on a table holding billions of rows.
// Run at startup rather than left to a manual migration: four collectors and a
// legal retention obligation is not a place for a step someone can forget.
var schemaDDL = []string{
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS nat_event UInt8 DEFAULT 0`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS username String DEFAULT ''`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS nat_dest_ip IPv4 DEFAULT toIPv4('0.0.0.0')`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS nat_dest_port UInt16 DEFAULT 0`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS exporter_flow_start Nullable(DateTime64(3))`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS exporter_flow_end Nullable(DateTime64(3))`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS collector_received DateTime64(3) DEFAULT now64(3)`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS src_ip_v6 IPv6 DEFAULT toIPv6('::')`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS dst_ip_v6 IPv6 DEFAULT toIPv6('::')`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS nat_public_ip_v6 IPv6 DEFAULT toIPv6('::')`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS nat_dest_ip_v6 IPv6 DEFAULT toIPv6('::')`,
	`ALTER TABLE %s.flow_logs ADD COLUMN IF NOT EXISTS exporter_ip_v6 IPv6 DEFAULT toIPv6('::')`,
}

const (
	sendTimeout = 30 * time.Second
	tickEvery   = 100 * time.Millisecond
	// drainGrace is how long a retiring shard keeps consuming after its quit
	// signal once its queue looks empty, so an enqueue that straddled a reload
	// pool-swap is still flushed rather than stranded in the retired channel.
	drainGrace = 200 * time.Millisecond
)

// appendError marks a deterministic per-row append failure (not worth retrying).
type appendError struct{ err error }

func (e *appendError) Error() string { return e.err.Error() }
func (e *appendError) Unwrap() error { return e.err }

// Manager owns the ClickHouse connection and the current writer pool, exposing a
// stable Enqueue across pool rebuilds.
type Manager struct {
	conn            driver.Conn
	connMu          sync.RWMutex
	connectMu       sync.Mutex
	ch              config.ClickHouseConfig
	live            *config.Store
	metrics         *metrics.Metrics
	log             *slog.Logger
	insert          string
	shutdownTimeout time.Duration

	mu   sync.Mutex // serializes Reload/Stop pool swaps
	pool atomic.Pointer[pool]

	// lastErr retains the most recent insert failure so the ingest-health
	// monitor can put the actual ClickHouse error in its alert email.
	lastErr atomic.Pointer[insertFailure]

	retireWG sync.WaitGroup // background drains of pools retired by Reload
	aggStop  chan struct{}
	aggWG    sync.WaitGroup

	// spool is the pre-insert batch WAL. nil restores the old unprotected
	// send/retry/drop behaviour.
	spool     *spool.Spool
	replayWG  sync.WaitGroup
	replayEnd chan struct{}
}

type insertFailure struct {
	msg string
	at  time.Time
}

// LastInsertError returns the most recent batch-insert failure and when it
// happened. Zero values mean no insert has failed since start.
func (m *Manager) LastInsertError() (string, time.Time) {
	if f := m.lastErr.Load(); f != nil {
		return f.msg, f.at
	}
	return "", time.Time{}
}

// compressionMethod maps the config string to a clickhouse compression method.
func compressionMethod(s string) clickhouse.CompressionMethod {
	switch s {
	case "none":
		return clickhouse.CompressionNone
	case "zstd":
		return clickhouse.CompressionZSTD
	case "lz4hc":
		return clickhouse.CompressionLZ4HC
	default:
		return clickhouse.CompressionLZ4
	}
}

// Connect opens and pings a ClickHouse connection. Exported for reuse by the
// archive exporter.
func Connect(ch config.ClickHouseConfig) (driver.Conn, error) {
	maxConns := ch.MaxOpenConns
	if maxConns <= 0 {
		maxConns = ch.WriterWorkers*2 + 2
		if maxConns < 8 {
			maxConns = 8
		}
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:         []string{ch.Addr},
		Auth:         clickhouse.Auth{Database: ch.Database, Username: ch.Username, Password: ch.Password},
		Compression:  &clickhouse.Compression{Method: compressionMethod(ch.Compression)},
		DialTimeout:  5 * time.Second,
		MaxOpenConns: maxConns,
		MaxIdleConns: ch.WriterWorkers,
	})
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse %s: %w", ch.Addr, err)
	}
	return conn, nil
}

// NewManager prepares the WAL and starts the writer pool. It connects to
// ClickHouse eagerly when possible; with a configured WAL it can start in
// WAL-first mode and reconnect lazily instead of refusing UDP traffic.
func NewManager(ch config.ClickHouseConfig, live *config.Store, m *metrics.Metrics, log *slog.Logger) (*Manager, error) {
	mgr := &Manager{
		ch:              ch,
		live:            live,
		metrics:         m,
		log:             log.With("component", "clickhouse"),
		insert:          fmt.Sprintf(insertStmt, ch.Database),
		shutdownTimeout: ch.ShutdownDrain(),
		aggStop:         make(chan struct{}),
	}
	if dir, maxBytes := ch.Spool(); dir != "" {
		sp, serr := spool.New(dir, maxBytes)
		if serr != nil {
			// Refuse to start rather than run without the durability the operator
			// configured. A collector that silently falls back to dropping batches
			// is how eight days went missing in the first place.
			return nil, fmt.Errorf("spool: %w", serr)
		}
		mgr.spool = sp
		if files, bytes, _ := sp.Stats(); files > 0 {
			mgr.log.Warn("spool holds batches from a previous run; replaying", "files", files, "bytes", bytes, "dir", dir)
		}
	} else {
		mgr.log.Warn("clickhouse.spool_dir is not set: batches are not write-ahead protected and failed inserts will be lost permanently")
	}
	if _, err := mgr.connection(); err != nil {
		if mgr.spool == nil {
			return nil, err
		}
		// WAL-first startup matters when ClickHouse itself is the reason natlog
		// was restarted. Writer attempts and replay will reconnect automatically;
		// refusing to bind UDP here would discard every packet in the meantime.
		mgr.log.Error("ClickHouse unavailable at startup; collector continuing in WAL-first mode", "error", err)
	}
	p := newPool(mgr, ch.WriterWorkers, ch.MaxQueueRows)
	p.start()
	mgr.pool.Store(p)
	mgr.aggWG.Add(1)
	go mgr.aggregate()
	if mgr.spool != nil {
		mgr.replayEnd = make(chan struct{})
		mgr.replayWG.Add(1)
		go mgr.replay()
	}
	return mgr, nil
}

// connection returns a schema-ready ClickHouse connection, creating it on
// demand when startup happened in WAL-first mode. connectMu prevents every
// writer shard from opening its own replacement during an outage.
func (m *Manager) connection() (driver.Conn, error) {
	m.connMu.RLock()
	conn := m.conn
	m.connMu.RUnlock()
	if conn != nil {
		return conn, nil
	}

	m.connectMu.Lock()
	defer m.connectMu.Unlock()
	m.connMu.RLock()
	conn = m.conn
	m.connMu.RUnlock()
	if conn != nil {
		return conn, nil
	}

	conn, err := Connect(m.ch)
	if err != nil {
		return nil, err
	}
	// Schema first: the insert statement names columns that an older table does
	// not have yet, and every insert would fail until it does.
	for _, stmt := range schemaDDL {
		sctx, scancel := context.WithTimeout(context.Background(), 60*time.Second)
		derr := conn.Exec(sctx, fmt.Sprintf(stmt, m.ch.Database))
		scancel()
		if derr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("flow_logs schema: %w", derr)
		}
	}
	m.connMu.Lock()
	m.conn = conn
	m.connMu.Unlock()
	m.log.Info("ClickHouse connection ready", "addr", m.ch.Addr)
	return conn, nil
}

// replayEvery paces spool drains. Short enough that a brief outage clears almost
// immediately, long enough that a sustained one does not spin against a database
// that is still refusing writes.
const replayEvery = 5 * time.Second

// replayMaxAttempts is how often one batch may fail before it is quarantined so
// the rest can proceed. It stays on disk either way — this is evidence, and
// deleting it to unblock a queue would repeat the mistake the spool corrects.
const replayMaxAttempts = 10

// replay drains the spool back into ClickHouse whenever it will take rows.
func (mgr *Manager) replay() {
	defer mgr.replayWG.Done()
	t := time.NewTicker(replayEvery)
	defer t.Stop()
	fails := map[string]int{}
	for {
		select {
		case <-mgr.replayEnd:
			return
		case <-t.C:
		}
		for {
			files, bytes, _ := mgr.spool.Stats()
			mgr.metrics.SpoolFiles.Set(float64(files))
			mgr.metrics.SpoolBytes.Set(float64(bytes))
			mgr.metrics.SpoolOldestSeconds.Set(mgr.spool.Age().Seconds())
			if files == 0 {
				break
			}
			name, batch, ok, err := mgr.spool.Oldest()
			if err != nil {
				// Undecodable: quarantine so it cannot block everything behind it.
				if name != "" {
					mgr.log.Error("spool batch unreadable; quarantining", "file", name, "error", err)
					if qerr := mgr.spool.Quarantine(name); qerr != nil {
						mgr.log.Error("unreadable spool batch could not be quarantined", "file", name, "error", qerr)
					}
				}
				break
			}
			if !ok {
				break
			}
			dedupToken := mgr.spool.DedupToken(name)
			if ierr := mgr.insertBatch(batch, dedupToken); ierr != nil {
				var ae *appendError
				if errors.As(ierr, &ae) {
					result := mgr.pool.Load().shards[0].salvage(batch, dedupToken)
					if result.dropped == 0 {
						delete(fails, name)
						mgr.metrics.SpoolReplayed.Add(float64(result.kept))
						if result.rejected > 0 {
							mgr.log.Error("spool batch contains rows ClickHouse cannot encode; quarantining after salvage",
								"file", name, "kept", result.kept, "rejected", result.rejected)
							if qerr := mgr.spool.Quarantine(name); qerr != nil {
								mgr.log.Error("could not quarantine rejected spool batch", "file", name, "error", qerr)
								break
							}
						} else if derr := mgr.spool.Done(name); derr != nil {
							mgr.log.Error("salvaged spool file inserted but not removed; it may be replayed again", "file", name, "error", derr)
							break
						}
						continue
					}
				}
				fails[name]++
				if fails[name] >= replayMaxAttempts {
					mgr.log.Error("spool batch refused repeatedly; quarantining so replay can continue",
						"file", name, "rows", len(batch), "attempts", fails[name], "error", ierr)
					if qerr := mgr.spool.Quarantine(name); qerr != nil {
						mgr.log.Error("spool batch could not be quarantined", "file", name, "error", qerr)
						break
					}
					delete(fails, name)
					continue
				}
				// ClickHouse is still unwell: stop and wait for the next tick.
				break
			}
			delete(fails, name)
			mgr.metrics.SpoolReplayed.Add(float64(len(batch)))
			mgr.metrics.FlowsInserted.Add(float64(len(batch)))
			if derr := mgr.spool.Done(name); derr != nil {
				// Left on disk after a successful insert: replaying it again would
				// duplicate rows. Delivery is at-least-once by design, but say so.
				mgr.log.Error("spool file inserted but not removed; it may be replayed again", "file", name, "error", derr)
				break
			}
			select {
			case <-mgr.replayEnd:
				return
			default:
			}
		}
	}
}

// insertBatch writes one batch directly, bypassing the shard queues. Used by
// replay so recovered records do not compete with live ingest for queue space.
func (mgr *Manager) insertBatch(batch []normalizer.FlowRecord, token string) error {
	if len(batch) == 0 {
		return nil
	}
	p := mgr.pool.Load()
	if p == nil || len(p.shards) == 0 {
		return fmt.Errorf("no writer pool")
	}
	return p.shards[0].send(batch, token)
}

// Enqueue submits rec to its shard, returning false if it was dropped.
func (mgr *Manager) Enqueue(rec normalizer.FlowRecord) bool {
	return mgr.pool.Load().enqueue(rec)
}

// QueueLen returns the total records buffered across all writer queues.
func (mgr *Manager) QueueLen() int { return mgr.pool.Load().queueLen() }

// Reload rebuilds the pool if the worker count or queue capacity changed: a new
// pool is started and swapped in atomically, and the old one drains in the
// background. Batch size, flush interval, retries and backpressure are read live
// and need no rebuild.
func (mgr *Manager) Reload(workers, queueRows int) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	cur := mgr.pool.Load()
	if cur.n == workers && cur.queueRows == queueRows {
		return
	}
	np := newPool(mgr, workers, queueRows)
	np.start()
	mgr.pool.Store(np)
	mgr.log.Info("writer pool rebuilt", "workers", workers, "queue_rows", queueRows)
	// Drain the retired pool off the hot path; Stop waits for these so the
	// ClickHouse connection is not closed underneath an in-flight flush.
	mgr.retireWG.Add(1)
	go func() {
		defer mgr.retireWG.Done()
		cur.drain(mgr.shutdownTimeout)
	}()
}

// Stop drains all writer queues (bounded by ShutdownTimeout) and closes the
// ClickHouse connection. Callers must stop the producers (receivers) first.
func (mgr *Manager) Stop() {
	mgr.mu.Lock()
	p := mgr.pool.Load()
	mgr.mu.Unlock()
	close(mgr.aggStop)
	mgr.aggWG.Wait()
	// Stop replay before draining: it must not be inserting through a connection
	// that is about to close, and the drain below may add fresh spool files of
	// its own if ClickHouse is unavailable at shutdown.
	if mgr.replayEnd != nil {
		close(mgr.replayEnd)
		mgr.replayWG.Wait()
	}
	p.drain(mgr.shutdownTimeout)
	mgr.retireWG.Wait() // let any pools retired by Reload finish flushing
	if mgr.spool != nil {
		if files, bytes, _ := mgr.spool.Stats(); files > 0 {
			// Not a failure — this is the spool doing its job. Say it plainly so
			// nobody wipes the directory thinking it is scratch space.
			mgr.log.Warn("shutting down with batches still spooled; they will be replayed on next start",
				"files", files, "bytes", bytes, "dir", mgr.spool.Dir())
		}
	}
	mgr.connMu.Lock()
	conn := mgr.conn
	mgr.conn = nil
	mgr.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// aggregate periodically publishes the aggregate queue depth.
func (mgr *Manager) aggregate() {
	defer mgr.aggWG.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-mgr.aggStop:
			mgr.metrics.QueueSize.Set(0)
			return
		case <-t.C:
			mgr.metrics.QueueSize.Set(float64(mgr.pool.Load().queueLen()))
		}
	}
}

// pool is a fixed set of shard writers.
type pool struct {
	mgr       *Manager
	n         int
	queueRows int
	shards    []*shard
	wg        sync.WaitGroup
}

func newPool(mgr *Manager, n, queueRows int) *pool {
	p := &pool{mgr: mgr, n: n, queueRows: queueRows, shards: make([]*shard, n)}
	for i := 0; i < n; i++ {
		p.shards[i] = &shard{
			mgr:   mgr,
			id:    i,
			label: strconv.Itoa(i),
			ch:    make(chan normalizer.FlowRecord, queueRows),
			quit:  make(chan struct{}),
		}
	}
	return p
}

func (p *pool) start() {
	for _, s := range p.shards {
		p.wg.Add(1)
		go func(s *shard) {
			defer p.wg.Done()
			s.run()
		}(s)
	}
}

func (p *pool) enqueue(rec normalizer.FlowRecord) bool {
	return p.shards[shardIndex(rec, p.n)].enqueue(rec)
}

func (p *pool) queueLen() int {
	total := 0
	for _, s := range p.shards {
		total += len(s.ch)
	}
	return total
}

// drain signals every shard to stop, then waits up to timeout for them to flush.
func (p *pool) drain(timeout time.Duration) {
	for _, s := range p.shards {
		close(s.quit)
	}
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		p.mgr.log.Error("writer drain timed out", "buffered", p.queueLen(), "timeout", timeout)
	}
}

// shard is a single writer with its own queue.
type shard struct {
	mgr   *Manager
	id    int
	label string
	ch    chan normalizer.FlowRecord // never closed; lifecycle signaled via quit
	quit  chan struct{}
}

func (s *shard) enqueue(rec normalizer.FlowRecord) bool {
	switch s.mgr.live.Load().Backpressure {
	case config.Block:
		select {
		case s.ch <- rec:
			s.publishSize()
			return true
		case <-s.quit:
			s.drop()
			return false
		}
	case config.DropOld:
		select {
		case s.ch <- rec:
			s.publishSize()
			return true
		case <-s.quit:
			s.drop()
			return false
		default:
			select { // evict oldest to make room
			case <-s.ch:
				s.drop()
			default:
			}
			select {
			case s.ch <- rec:
				s.publishSize()
				return true
			default:
				s.drop()
				return false
			}
		}
	default: // DropNew
		select {
		case s.ch <- rec:
			s.publishSize()
			return true
		case <-s.quit:
			s.drop()
			return false
		default:
			s.drop()
			return false
		}
	}
}

func (s *shard) drop() {
	s.mgr.metrics.WriterQueueDropped.WithLabelValues(s.label).Inc()
	s.mgr.metrics.FlowsDropped.Inc()
}

func (s *shard) publishSize() {
	s.mgr.metrics.WriterQueueSize.WithLabelValues(s.label).Set(float64(len(s.ch)))
}

func (s *shard) run() {
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	batch := make([]normalizer.FlowRecord, 0, 1024)
	lastFlush := time.Now()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flush(batch)
		batch = batch[:0]
		lastFlush = time.Now()
		s.publishSize()
	}

	for {
		select {
		case rec := <-s.ch:
			batch = append(batch, rec)
			if len(batch) >= s.mgr.live.Load().BatchSize {
				flush()
			}
		case <-ticker.C:
			if live := s.mgr.live.Load(); len(batch) > 0 && time.Since(lastFlush) >= live.FlushInterval {
				flush()
			}
			s.publishSize()
		case <-s.quit:
			// Keep consuming until the queue stays empty for drainGrace, so an
			// enqueue that straddled a reload pool-swap is still flushed, then
			// flush and exit.
			idle := time.NewTimer(drainGrace)
			for {
				select {
				case rec := <-s.ch:
					batch = append(batch, rec)
					if len(batch) >= s.mgr.live.Load().BatchSize {
						flush()
					}
					if !idle.Stop() {
						select {
						case <-idle.C:
						default:
						}
					}
					idle.Reset(drainGrace)
				case <-idle.C:
					flush()
					idle.Stop()
					return
				}
			}
		}
	}
}

func (s *shard) flush(batch []normalizer.FlowRecord) {
	live := s.mgr.live.Load()
	attempts := live.RetryMaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	start := time.Now()
	walName := ""
	walToken := ""
	if s.mgr.spool != nil {
		var werr error
		walName, werr = s.mgr.spool.Begin(batch)
		if werr == nil {
			s.mgr.metrics.SpoolSaved.Add(float64(len(batch)))
			walToken = s.mgr.spool.DedupToken(walName)
		} else if werr == spool.ErrFull {
			if s.mgr.spool.MarkFull() {
				files, bytes, _ := s.mgr.spool.Stats()
				s.mgr.log.Error("WRITE-AHEAD LOG FULL — attempting ClickHouse without crash protection",
					"dir", s.mgr.spool.Dir(), "files", files, "bytes", bytes)
			}
		} else {
			s.mgr.log.Error("write-ahead log failed; attempting ClickHouse without crash protection",
				"worker", s.id, "rows", len(batch), "error", werr)
		}
	}
	var err error
	for a := 1; a <= attempts; a++ {
		if err = s.send(batch, walToken); err == nil {
			s.mgr.metrics.WriterInsertLatency.Observe(float64(time.Since(start).Milliseconds()))
			s.mgr.metrics.WriterBatches.Inc()
			s.mgr.metrics.WriterBatchRows.Add(float64(len(batch)))
			s.mgr.metrics.FlowsInserted.Add(float64(len(batch)))
			if walName != "" {
				if derr := s.mgr.spool.Done(walName); derr != nil {
					// The active WAL is deliberately retained. A restart may replay
					// it, but deleting evidence without a durable unlink would be worse.
					s.mgr.log.Error("insert succeeded but active WAL could not be removed; restart may replay it",
						"worker", s.id, "file", walName, "error", derr)
				}
			}
			return
		}
		var ae *appendError
		if errors.As(err, &ae) {
			result := s.salvage(batch, walToken)
			if walName != "" {
				s.finishSalvageWAL(walName, result)
			} else if result.dropped > 0 {
				s.mgr.metrics.FlowsDropped.Add(float64(result.dropped))
			}
			return
		}
		s.mgr.metrics.WriterRetries.Inc()
		s.mgr.lastErr.Store(&insertFailure{msg: err.Error(), at: time.Now()})
		s.mgr.log.Warn("batch insert failed", "worker", s.id, "attempt", a, "rows", len(batch), "error", err)
		if a < attempts {
			time.Sleep(backoff(a, live.RetryBackoff))
		}
	}
	s.mgr.metrics.InsertErrors.Inc()
	// Retries are exhausted. A normal write-ahead batch is already durable and
	// only needs to be released to replay; Save remains as a fallback when the
	// pre-insert WAL write itself failed.
	if s.mgr.spool != nil {
		if walName != "" {
			if rerr := s.mgr.spool.Ready(walName); rerr == nil {
				s.mgr.log.Warn("clickhouse refused the write-ahead batch; released it for replay",
					"worker", s.id, "file", walName, "rows", len(batch), "error", err)
				return
			} else {
				// It remains as .active and New will adopt it after a restart. It is
				// not counted as lost, but live replay cannot see it yet.
				s.mgr.log.Error("write-ahead batch is durable but could not be released for live replay",
					"worker", s.id, "file", walName, "rows", len(batch), "error", rerr)
				return
			}
		}
		if serr := s.mgr.spool.Save(batch); serr == nil {
			s.mgr.metrics.SpoolSaved.Add(float64(len(batch)))
			s.mgr.log.Warn("clickhouse refused the batch; fallback spool write succeeded",
				"worker", s.id, "rows", len(batch), "error", err)
			return
		} else if serr == spool.ErrFull {
			if s.mgr.spool.MarkFull() {
				files, bytes, _ := s.mgr.spool.Stats()
				s.mgr.log.Error("SPOOL FULL — records are now being lost permanently. Restore ClickHouse or raise clickhouse.spool_max_gb",
					"dir", s.mgr.spool.Dir(), "files", files, "bytes", bytes)
			}
		} else {
			s.mgr.log.Error("spool write failed; records will be lost", "worker", s.id, "rows", len(batch), "error", serr)
		}
	}
	s.mgr.metrics.SpoolLost.Add(float64(len(batch)))
	s.mgr.metrics.FlowsDropped.Add(float64(len(batch)))
	s.mgr.log.Error("dropping batch after exhausting retries — these records cannot be recovered",
		"worker", s.id, "rows", len(batch), "error", err)
}

type salvageResult struct {
	kept     int
	rejected int
	dropped  int
}

// salvage re-inserts a rejected batch one row at a time, keeping the good rows.
func (s *shard) salvage(batch []normalizer.FlowRecord, tokenBase string) salvageResult {
	result := salvageResult{}
	for i := range batch {
		token := ""
		if tokenBase != "" {
			token = tokenBase + ":" + strconv.Itoa(i)
		}
		err := s.send(batch[i:i+1], token)
		if err == nil {
			result.kept++
			continue
		}
		var ae *appendError
		if errors.As(err, &ae) {
			result.rejected++ // genuine per-row rejection (bad/incompatible data)
		} else {
			result.dropped++ // transient prepare/send failure, not a rejection
		}
	}
	if result.kept > 0 {
		s.mgr.metrics.FlowsInserted.Add(float64(result.kept))
		s.mgr.metrics.WriterBatchRows.Add(float64(result.kept))
	}
	if result.rejected > 0 {
		s.mgr.metrics.FlowsRejected.Add(float64(result.rejected))
	}
	if result.rejected+result.dropped > 0 {
		s.mgr.log.Error("salvage incomplete", "worker", s.id, "kept", result.kept, "rejected", result.rejected, "transient", result.dropped)
	}
	return result
}

func (s *shard) finishSalvageWAL(name string, result salvageResult) {
	switch {
	case result.dropped > 0:
		// Some rows hit a transient send failure. Release the original batch;
		// stable per-row tokens make subsequent salvage retries idempotent on a
		// ReplicatedMergeTree within ClickHouse's deduplication window.
		if err := s.mgr.spool.Ready(name); err != nil {
			s.mgr.log.Error("salvage WAL could not be released for replay", "file", name, "error", err)
		}
	case result.rejected > 0:
		// Good rows landed, while permanently invalid rows remain available as
		// evidence without blocking every later WAL batch.
		if err := s.mgr.spool.Quarantine(name); err != nil {
			s.mgr.log.Error("salvage WAL could not be quarantined", "file", name, "error", err)
		}
	default:
		if err := s.mgr.spool.Done(name); err != nil {
			s.mgr.log.Error("salvaged WAL could not be removed", "file", name, "error", err)
		}
	}
}

// insertSettings returns the per-INSERT ClickHouse settings for async inserts,
// or nil when async inserts are disabled.
func insertSettings(live *config.Live, dedupToken string) clickhouse.Settings {
	var settings clickhouse.Settings
	if live.AsyncInsert {
		settings = clickhouse.Settings{"async_insert": 1}
		if live.WaitForAsyncInsert {
			settings["wait_for_async_insert"] = 1
		} else {
			settings["wait_for_async_insert"] = 0
		}
		if live.AsyncInsertBusyTimeoutMS > 0 {
			settings["async_insert_busy_timeout_ms"] = live.AsyncInsertBusyTimeoutMS
		}
	}
	if dedupToken != "" {
		if settings == nil {
			settings = clickhouse.Settings{}
		}
		settings["insert_deduplication_token"] = dedupToken
	}
	return settings
}

func (s *shard) send(batch []normalizer.FlowRecord, dedupToken string) error {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if set := insertSettings(s.mgr.live.Load(), dedupToken); set != nil {
		ctx = clickhouse.Context(ctx, clickhouse.WithSettings(set))
	}
	conn, err := s.mgr.connection()
	if err != nil {
		return fmt.Errorf("connect clickhouse: %w", err)
	}
	b, err := conn.PrepareBatch(ctx, s.mgr.insert)
	if err != nil {
		return fmt.Errorf("prepare batch: %w", err)
	}
	for i := range batch {
		r := &batch[i]
		if err := b.Append(
			r.ISPID, r.DeviceID,
			ip4(r.SrcIP), r.SrcPort, ip4(r.DstIP), r.DstPort,
			ip4(r.NatPublicIP), r.NatPublicPort, ip4(r.NatDestIP), r.NatDestPort, r.NatEvent, r.Username,
			r.Protocol, r.Bytes, r.Packets,
			r.FlowStart, r.FlowEnd, nullableTime(r.ExporterFlowStart), nullableTime(r.ExporterFlowEnd), r.CollectorReceived,
			r.FlowType, ip4(r.ExporterIP), ip6(r.SrcIP), ip6(r.DstIP), ip6(r.NatPublicIP), ip6(r.NatDestIP), ip6(r.ExporterIP),
		); err != nil {
			_ = b.Abort()
			return &appendError{fmt.Errorf("append row %d: %w", i, err)}
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("send batch: %w", err)
	}
	return nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
func ip6(ip net.IP) net.IP {
	if ip == nil || ip.To4() != nil {
		return net.IPv6zero
	}
	if v := ip.To16(); v != nil {
		return v
	}
	return net.IPv6zero
}

// shardIndex routes a record to a shard by hashing (exporter_ip, device_id) so a
// given exporter/device always lands on the same writer.
func shardIndex(rec normalizer.FlowRecord, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	if v4 := rec.ExporterIP.To4(); v4 != nil {
		_, _ = h.Write(v4)
	} else if rec.ExporterIP != nil {
		_, _ = h.Write(rec.ExporterIP)
	}
	var d [4]byte
	d[0] = byte(rec.DeviceID)
	d[1] = byte(rec.DeviceID >> 8)
	d[2] = byte(rec.DeviceID >> 16)
	d[3] = byte(rec.DeviceID >> 24)
	_, _ = h.Write(d[:])
	// Lemire multiply-shift reduction uses the well-mixed high bits of the hash;
	// plain `% n` would key off the low bits, which barely vary when the trailing
	// (device high) bytes are constant zero — clustering records onto few shards.
	return int((uint64(h.Sum32()) * uint64(n)) >> 32)
}

func backoff(attempt int, base time.Duration) time.Duration {
	d := time.Duration(attempt) * base
	if max := 2 * time.Second; d > max {
		d = max
	}
	return d
}

// ip4 returns a 4-byte IPv4 address, substituting 0.0.0.0 for nil/non-IPv4.
func ip4(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return net.IPv4zero.To4()
}
