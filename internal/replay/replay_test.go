package replay_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pcapio"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	"github.com/natflow/natflow-dataplane/internal/replay"
	"github.com/natflow/natflow-dataplane/internal/rules"
	"github.com/natflow/natflow-dataplane/internal/sanitize"
)

type capWriter struct {
	mu sync.Mutex
	n  int
}

func (c *capWriter) Enqueue(normalizer.FlowRecord) bool {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return true
}
func (c *capWriter) count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.n }

func v5Record() []byte {
	r := make([]byte, 48)
	copy(r[0:4], net.IPv4(10, 0, 0, 5).To4())
	copy(r[4:8], net.IPv4(1, 1, 1, 1).To4())
	binary.BigEndian.PutUint32(r[16:20], 4)
	binary.BigEndian.PutUint32(r[20:24], 800) // bytes > 0
	binary.BigEndian.PutUint16(r[32:34], 40000)
	binary.BigEndian.PutUint16(r[34:36], 443)
	r[38] = 6
	return r
}

func v5Packet(n int) []byte {
	h := make([]byte, 24)
	binary.BigEndian.PutUint16(h[0:2], 5)
	binary.BigEndian.PutUint16(h[2:4], uint16(n))
	binary.BigEndian.PutUint32(h[8:12], 1_700_000_000)
	out := h
	for i := 0; i < n; i++ {
		out = append(out, v5Record()...)
	}
	return out
}

// TestReplaySanitizedFixtureIntoCollector builds a pcap, sanitizes it, replays it
// into a live receiver+pipeline, and verifies flows decode and reach the writer.
func TestReplaySanitizedFixtureIntoCollector(t *testing.T) {
	// 1. Build a pcap with 3 packets x 5 flows each.
	var raw bytes.Buffer
	pw, err := pcapio.NewWriter(&raw, pcapio.LinkEthernet)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		frame := pcapio.EthernetIPv4UDP([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, 40000, 2055, v5Packet(5))
		if err := pw.Write(pcapio.Packet{Time: time.Unix(1_700_000_000, int64(i)).UTC(), Data: frame}); err != nil {
			t.Fatal(err)
		}
	}

	// 2. Sanitize the pcap.
	var san bytes.Buffer
	rd, err := pcapio.NewReader(&raw)
	if err != nil {
		t.Fatal(err)
	}
	sw, err := pcapio.NewWriter(&san, rd.LinkType())
	if err != nil {
		t.Fatal(err)
	}
	s := sanitize.New([]byte("k"))
	for {
		p, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if s.Frame(rd.LinkType(), p.Data) {
			if err := sw.Write(p); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 3. Stand up a collector harness.
	m := metrics.New()
	cw := &capWriter{}
	live := config.NewStore(config.Live{Rules: rules.RuleSet{}}) // skip nothing
	reg, _ := device.Build(nil, rules.RuleSet{})
	devs := device.NewStore(reg)
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p := pipeline.New(netflow5.New(), normalizer.New(), live, devs, 1, 2, cw, m, log)
	rcv, err := receiver.New("nf5", "127.0.0.1", 0, 2, 0, p, m, log)
	if err != nil {
		t.Fatal(err)
	}
	rcv.Start()
	defer rcv.Stop()
	conn, err := net.DialUDP("udp", nil, rcv.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 4. Replay the sanitized fixture into the collector.
	st, err := replay.Stream(&san, replay.NetFlow5, 0, func(b []byte) error {
		_, e := conn.Write(b)
		return e
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if st.Sent != 3 {
		t.Fatalf("replayed %d payloads, want 3", st.Sent)
	}

	// 5. Verify flows decoded and consumed by the insert path.
	deadline := time.Now().Add(3 * time.Second)
	for cw.count() < 15 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cw.count() == 0 {
		t.Fatal("no records consumed from replayed fixture")
	}
	if v := testutil.ToFloat64(m.FlowsDecoded); v == 0 {
		t.Error("flows_decoded_total is 0 after replay")
	}
}

func TestParseProto(t *testing.T) {
	for in, want := range map[string]replay.Proto{"": replay.Any, "auto": replay.Any, "netflow5": replay.NetFlow5, "netflow9": replay.NetFlow9, "ipfix": replay.IPFIX} {
		if got, err := replay.ParseProto(in); err != nil || got != want {
			t.Errorf("ParseProto(%q)=%v,%v want %v", in, got, err, want)
		}
	}
	if _, err := replay.ParseProto("sflow"); err == nil {
		t.Error("want error for invalid proto")
	}
}
