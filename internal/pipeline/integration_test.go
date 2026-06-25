package pipeline_test

import (
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/natflow/natflow-dataplane/internal/decoder/ipfix"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	"github.com/natflow/natflow-dataplane/internal/rules"
)

// captureWriter stands in for the ClickHouse writer at the pipeline boundary.
type captureWriter struct {
	mu   sync.Mutex
	recs []normalizer.FlowRecord
}

func (c *captureWriter) Enqueue(r normalizer.FlowRecord) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recs = append(c.recs, r)
	return true
}

func (c *captureWriter) records() []normalizer.FlowRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]normalizer.FlowRecord, len(c.recs))
	copy(out, c.recs)
	return out
}

func v5Record(src, dst net.IP, sp, dp uint16, pkts, octets uint32, proto uint8) []byte {
	r := make([]byte, 48)
	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint32(r[16:20], pkts)
	binary.BigEndian.PutUint32(r[20:24], octets)
	binary.BigEndian.PutUint16(r[32:34], sp)
	binary.BigEndian.PutUint16(r[34:36], dp)
	r[38] = proto
	return r
}

func v5Packet(recs ...[]byte) []byte {
	hdr := make([]byte, 24)
	binary.BigEndian.PutUint16(hdr[0:2], 5)
	binary.BigEndian.PutUint16(hdr[2:4], uint16(len(recs)))
	binary.BigEndian.PutUint32(hdr[8:12], 1_700_000_000) // unix_secs
	pkt := hdr
	for _, r := range recs {
		pkt = append(pkt, r...)
	}
	return pkt
}

// TestEndToEndNetFlow5 exercises the full hot path: a real UDP listener +
// worker pool -> NetFlow v5 decoder -> normalizer -> skip rules -> writer,
// asserting metrics and the surviving record.
func TestEndToEndNetFlow5(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := metrics.New()
	w := &captureWriter{}

	p := pipeline.New(
		netflow5.New(),
		normalizer.New(42, 7),
		rules.New(true, true, true), // skip DNS / private->private / zero-byte
		w, m, log,
	)

	rcv, err := receiver.New("netflow5", "127.0.0.1", 0 /*ephemeral*/, 2, 0, p, m, log)
	if err != nil {
		t.Fatalf("receiver.New: %v", err)
	}
	rcv.Start()
	defer rcv.Stop()

	addr := rcv.LocalAddr().(*net.UDPAddr)
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Two flows: a normal egress HTTPS flow (kept) and a DNS flow (skipped).
	normal := v5Record(net.IPv4(10, 0, 0, 5), net.IPv4(1, 1, 1, 1), 40000, 443, 10, 1500, 6)
	dns := v5Record(net.IPv4(10, 0, 0, 5), net.IPv4(8, 8, 8, 8), 40001, 53, 2, 140, 17)
	if _, err := conn.Write(v5Packet(normal, dns)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Processing is asynchronous; poll until the surviving record arrives.
	deadline := time.Now().Add(3 * time.Second)
	for len(w.records()) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	recs := w.records()
	if len(recs) != 1 {
		t.Fatalf("want 1 enqueued record (DNS skipped), got %d", len(recs))
	}
	got := recs[0]
	if got.DstPort != 443 || got.Bytes != 1500 {
		t.Errorf("unexpected record: dstPort=%d bytes=%d", got.DstPort, got.Bytes)
	}
	if got.ISPID != 42 || got.DeviceID != 7 {
		t.Errorf("identity not stamped: isp=%d dev=%d", got.ISPID, got.DeviceID)
	}
	if got.FlowType != "netflow5" {
		t.Errorf("flowType = %q", got.FlowType)
	}
	if !got.ExporterIP.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Errorf("exporterIP = %v, want 127.0.0.1", got.ExporterIP)
	}

	if v := testutil.ToFloat64(m.PacketsReceived); v != 1 {
		t.Errorf("packets_received_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.FlowsDecoded); v != 2 {
		t.Errorf("flows_decoded_total = %v, want 2", v)
	}
	if v := testutil.ToFloat64(m.FlowsSkipped); v != 1 {
		t.Errorf("flows_skipped_total = %v, want 1", v)
	}
}

// TestUnsupportedProtocolCounted verifies that a structurally-valid but
// not-yet-decoded protocol (IPFIX in v1) is counted under packets_unsupported
// rather than packets_dropped.
func TestUnsupportedProtocolCounted(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := metrics.New()
	w := &captureWriter{}

	p := pipeline.New(ipfix.New(), normalizer.New(1, 0), rules.New(true, true, true), w, m, log)
	rcv, err := receiver.New("ipfix", "127.0.0.1", 0, 1, 0, p, m, log)
	if err != nil {
		t.Fatalf("receiver.New: %v", err)
	}
	rcv.Start()
	defer rcv.Stop()

	conn, err := net.DialUDP("udp", nil, rcv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Minimal valid IPFIX message header: 16 bytes with version field == 10.
	msg := make([]byte, 16)
	binary.BigEndian.PutUint16(msg[0:2], 10)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for testutil.ToFloat64(m.PacketsReceived) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Give the handler a moment to classify after receipt.
	time.Sleep(50 * time.Millisecond)

	if v := testutil.ToFloat64(m.PacketsUnsupported); v != 1 {
		t.Errorf("packets_unsupported_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.PacketsDropped); v != 0 {
		t.Errorf("packets_dropped_total = %v, want 0", v)
	}
	if len(w.records()) != 0 {
		t.Errorf("want 0 enqueued records, got %d", len(w.records()))
	}
}
