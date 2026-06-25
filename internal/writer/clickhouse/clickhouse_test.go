package clickhouse

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

func rec(ip string, dev uint32) normalizer.FlowRecord {
	return normalizer.FlowRecord{ExporterIP: net.ParseIP(ip), DeviceID: dev}
}

func TestShardIndexStableAndInRange(t *testing.T) {
	const n = 8
	a := shardIndex(rec("10.0.0.1", 5), n)
	b := shardIndex(rec("10.0.0.1", 5), n)
	if a != b {
		t.Fatalf("shardIndex not stable: %d != %d", a, b)
	}
	if a < 0 || a >= n {
		t.Fatalf("shardIndex out of range: %d", a)
	}
	if shardIndex(rec("10.0.0.1", 5), 1) != 0 {
		t.Error("single shard must be 0")
	}
}

func TestShardSpread(t *testing.T) {
	const n = 8
	seen := map[int]bool{}
	for i := 0; i < 200; i++ {
		r := rec(net.IPv4(10, byte(i/256), byte(i), 1).String(), uint32(i))
		seen[shardIndex(r, n)] = true
	}
	if len(seen) < n/2 {
		t.Errorf("poor shard spread: only %d/%d shards used", len(seen), n)
	}
}

func testManager(mode config.BackpressureMode) (*Manager, *metrics.Metrics) {
	m := metrics.New()
	return &Manager{
		live:    config.NewStore(config.Live{Backpressure: mode, BatchSize: 1000}),
		metrics: m,
		log:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}, m
}

func TestBackpressureDropNew(t *testing.T) {
	mgr, m := testManager(config.DropNew)
	p := newPool(mgr, 1, 2) // 1 shard, capacity 2, NOT started so nothing drains

	r := rec("10.0.0.1", 1)
	ok := 0
	for i := 0; i < 5; i++ {
		if p.enqueue(r) {
			ok++
		}
	}
	if ok != 2 {
		t.Fatalf("drop_new: accepted %d, want 2 (queue cap)", ok)
	}
	if v := testutil.ToFloat64(m.WriterQueueDropped.WithLabelValues("0")); v != 3 {
		t.Errorf("writer_queue_dropped{0} = %v, want 3", v)
	}
	if v := testutil.ToFloat64(m.FlowsDropped); v != 3 {
		t.Errorf("flows_dropped_total = %v, want 3", v)
	}
}

func TestBackpressureDropOld(t *testing.T) {
	mgr, m := testManager(config.DropOld)
	p := newPool(mgr, 1, 2)

	r := rec("10.0.0.1", 1)
	for i := 0; i < 5; i++ {
		if !p.enqueue(r) {
			t.Fatalf("drop_old should always accept the new record (i=%d)", i)
		}
	}
	// 5 enqueued, capacity 2 -> 3 oldest evicted.
	if v := testutil.ToFloat64(m.WriterQueueDropped.WithLabelValues("0")); v != 3 {
		t.Errorf("drop_old evictions = %v, want 3", v)
	}
	if got := len(p.shards[0].ch); got != 2 {
		t.Errorf("queue len = %d, want 2", got)
	}
}

// TestReloadEnqueueNoRace exercises concurrent Enqueue (pool.Load) against
// Reload-style pool swaps (pool.Store) so `go test -race` validates the atomic
// pool pointer. Pools are not started (no ClickHouse connection needed).
func TestInsertSettings(t *testing.T) {
	if s := insertSettings(&config.Live{AsyncInsert: false}); s != nil {
		t.Errorf("async off should yield nil settings, got %v", s)
	}
	s := insertSettings(&config.Live{AsyncInsert: true, WaitForAsyncInsert: true})
	if s["async_insert"] != 1 || s["wait_for_async_insert"] != 1 {
		t.Errorf("async+wait settings wrong: %v", s)
	}
	s = insertSettings(&config.Live{AsyncInsert: true, WaitForAsyncInsert: false, AsyncInsertBusyTimeoutMS: 200})
	if s["wait_for_async_insert"] != 0 {
		t.Errorf("fire-and-forget should set wait=0, got %v", s["wait_for_async_insert"])
	}
	if s["async_insert_busy_timeout_ms"] != 200 {
		t.Errorf("busy timeout not set: %v", s["async_insert_busy_timeout_ms"])
	}
}

func TestCompressionMethod(t *testing.T) {
	if compressionMethod("none") != clickhouse.CompressionNone {
		t.Error("none")
	}
	if compressionMethod("zstd") != clickhouse.CompressionZSTD {
		t.Error("zstd")
	}
	if compressionMethod("") != clickhouse.CompressionLZ4 {
		t.Error("default lz4")
	}
}

func TestReloadEnqueueNoRace(t *testing.T) {
	mgr, _ := testManager(config.DropNew)
	mgr.pool.Store(newPool(mgr, 2, 4))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := rec("10.0.0.1", 1)
			for {
				select {
				case <-stop:
					return
				default:
					mgr.Enqueue(r)
					_ = mgr.QueueLen()
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			select {
			case <-stop:
				return
			default:
				mgr.pool.Store(newPool(mgr, 1+j%4, 4))
				time.Sleep(time.Millisecond)
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}
