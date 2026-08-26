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

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	"github.com/natflow/natflow-dataplane/internal/rules"
)

func liveStore() *config.Store {
	return config.NewStore(config.Live{
		Rules: rules.RuleSet{SkipDNS: true, SkipPrivateToPrivate: true, SkipZeroBytes: true},
	})
}

func emptyDevices() *device.Store {
	r, _ := device.Build(nil, rules.RuleSet{})
	return device.NewStore(r)
}

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
// worker pool -> NetFlow v5 decoder -> normalizer -> skip rules -> writer.
//
// Nothing survives, and that is the point. NetFlow v5 has no post-NAT fields in
// its fixed record layout, so every v5 flow fails the hard-coded no-translation
// rule. A v5 exporter can therefore be received, decoded and counted while
// storing nothing at all — the single most surprising consequence of that rule,
// pinned here so it can never be discovered in production instead.
func TestEndToEndNetFlow5(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := metrics.New()
	w := &captureWriter{}

	p := pipeline.New(
		netflow5.New(),
		normalizer.New(),
		liveStore(),    // skip DNS / private->private / zero-byte
		emptyDevices(), // unmatched -> allow mode uses the defaults below
		42, 7,
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

	// Two flows: a normal egress HTTPS flow and a DNS flow. Both are dropped —
	// the first for carrying no translation, the second for being DNS.
	normal := v5Record(net.IPv4(10, 0, 0, 5), net.IPv4(1, 1, 1, 1), 40000, 443, 10, 1500, 6)
	dns := v5Record(net.IPv4(10, 0, 0, 5), net.IPv4(8, 8, 8, 8), 40001, 53, 2, 140, 17)
	if _, err := conn.Write(v5Packet(normal, dns)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Processing is asynchronous; wait for both flows to be accounted for.
	deadline := time.Now().Add(3 * time.Second)
	for testutil.ToFloat64(m.FlowsSkipped) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if recs := w.records(); len(recs) != 0 {
		t.Fatalf("NetFlow v5 carries no post-NAT fields; want 0 stored records, got %d", len(recs))
	}
	if v := testutil.ToFloat64(m.PacketsReceived); v != 1 {
		t.Errorf("packets_received_total = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.FlowsDecoded); v != 2 {
		t.Errorf("flows_decoded_total = %v, want 2 (decoding is unaffected)", v)
	}
	if v := testutil.ToFloat64(m.FlowsSkipped); v != 2 {
		t.Errorf("flows_skipped_total = %v, want 2", v)
	}
}

// A v5 exporter storing nothing must still be visibly alive, or an operator
// reads "no records" as a dead link and goes looking for a network fault.
func TestNetFlow5LeavesEvidenceItWasAlive(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := metrics.New()
	w := &captureWriter{}
	sig := pipeline.NewDeviceSignals()

	// A registered exporter, as production runs it (unknown_exporter_mode:
	// reject) — evidence is kept for devices the operator actually manages.
	built, err := device.Build([]device.Spec{{
		Name: "v5-nas", Enabled: true, ExporterIP: "127.0.0.1", ISPID: 42, DeviceID: 7,
	}}, rules.RuleSet{SkipDNS: true, SkipPrivateToPrivate: true, SkipZeroBytes: true})
	if err != nil {
		t.Fatalf("device.Build: %v", err)
	}
	devs := device.NewStore(built)
	live := config.NewStore(config.Live{
		UnknownMode: device.ModeReject,
		Rules:       rules.RuleSet{SkipDNS: true, SkipPrivateToPrivate: true, SkipZeroBytes: true},
	})

	p := pipeline.New(netflow5.New(), normalizer.New(), live, devs, 42, 7, w, m, log)
	p.SetDeviceSignals(sig)

	rcv, err := receiver.New("netflow5", "127.0.0.1", 0, 2, 0, p, m, log)
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
	if _, err := conn.Write(v5Packet(v5Record(net.IPv4(10, 0, 0, 5), net.IPv4(1, 1, 1, 1), 40000, 443, 10, 1500, 6))); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := sig.Snapshot()[7]; ok && s.NoNATDropped > 0 {
			if s.LastFlow.IsZero() {
				t.Error("evidence recorded without a timestamp; liveness cannot use it")
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no evidence recorded for device 7; a live exporter would look dead. snapshot=%+v", sig.Snapshot())
}

// unsupportedDecoder always reports decoder.ErrUnsupported, standing in for a
// recognized-but-undecodable protocol.
type unsupportedDecoder struct{}

func (unsupportedDecoder) Kind() string { return "fake" }
func (unsupportedDecoder) Decode(dst []decoder.Flow, _ []byte, _ net.IP) ([]decoder.Flow, error) {
	return dst, decoder.ErrUnsupported
}

// TestUnsupportedProtocolCounted verifies that a recognized-but-undecodable
// protocol is counted under packets_unsupported rather than packets_dropped.
func TestUnsupportedProtocolCounted(t *testing.T) {
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	m := metrics.New()
	w := &captureWriter{}

	p := pipeline.New(unsupportedDecoder{}, normalizer.New(), liveStore(), emptyDevices(), 1, 0, w, m, log)
	rcv, err := receiver.New("fake", "127.0.0.1", 0, 1, 0, p, m, log)
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

	if _, err := conn.Write([]byte{0xde, 0xad, 0xbe, 0xef}); err != nil {
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
