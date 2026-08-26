package normalizer

import (
	"net"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

func TestNormalize(t *testing.T) {
	n := New()
	start := time.Now().Add(-time.Minute) // recent → plausible, kept as-is
	f := decoder.Flow{
		SrcIP:      net.IPv4(10, 0, 0, 1).To4(),
		DstIP:      net.IPv4(8, 8, 8, 8).To4(),
		SrcPort:    100,
		DstPort:    200,
		Protocol:   6,
		Bytes:      500,
		Packets:    4,
		FlowStart:  start,
		FlowEnd:    start.Add(time.Second),
		ExporterIP: net.IPv4(192, 168, 0, 1).To4(),
	}
	r := n.Normalize(f, "netflow5", 7, 3)
	if r.ISPID != 7 || r.DeviceID != 3 {
		t.Errorf("identity = %d/%d, want 7/3", r.ISPID, r.DeviceID)
	}
	if r.FlowType != "netflow5" {
		t.Errorf("flowType = %q", r.FlowType)
	}
	if r.Bytes != 500 || r.Packets != 4 {
		t.Errorf("bytes/pkts = %d/%d", r.Bytes, r.Packets)
	}
	if r.Protocol != 6 || r.SrcPort != 100 || r.DstPort != 200 {
		t.Errorf("proto/ports = %d %d/%d", r.Protocol, r.SrcPort, r.DstPort)
	}
	if !r.SrcIP.Equal(f.SrcIP) || !r.DstIP.Equal(f.DstIP) {
		t.Error("ip mismatch")
	}
	// The exporter said the flow started a minute ago. That is ignored on
	// purpose: the stored time is the collector's.
	if r.FlowStart.Equal(start) {
		t.Error("the exporter's flow time was used; the collector's clock must be")
	}
	if d := time.Since(r.FlowStart); d < 0 || d > 2*time.Second {
		t.Errorf("FlowStart = %v, want the collector's current time", r.FlowStart)
	}
	if !r.FlowEnd.Equal(r.FlowStart) {
		t.Errorf("FlowEnd = %v, want it equal to FlowStart", r.FlowEnd)
	}
}

// Exporter clocks are not trustworthy — one on this fleet runs 38.5 hours behind
// — and a store holding a mixture of good and bad clocks cannot produce a
// defensible timeline: two records a minute apart on the wire land days apart in
// the table, and nothing downstream can tell which is which. Every record is
// stamped with the collector's clock, whatever the exporter claims.
func TestExporterClockIsNeverUsed(t *testing.T) {
	n := New()
	now := time.Now()
	for _, c := range []struct {
		name  string
		start time.Time
	}{
		{"no time IE at all", time.Time{}},
		{"38 hours behind (the Wadhai case)", now.Add(-38*time.Hour - 30*time.Minute)},
		{"a couple of minutes behind", now.Add(-2 * time.Minute)},
		{"seconds behind", now.Add(-3 * time.Second)},
		{"ahead of us", now.Add(10 * time.Minute)},
		{"absurdly ahead", now.Add(500 * time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := n.Normalize(decoder.Flow{FlowStart: c.start, FlowEnd: c.start.Add(time.Hour)}, "netflow9", 1, 1)
			if d := time.Since(r.FlowStart); d < 0 || d > 2*time.Second {
				t.Errorf("FlowStart = %v, want the collector's current time", r.FlowStart)
			}
			if !r.FlowEnd.Equal(r.FlowStart) {
				t.Errorf("FlowEnd = %v, want it equal to FlowStart", r.FlowEnd)
			}
		})
	}
}

// Two flows the exporter dates days apart must land in collector order, because
// that ordering is what a request about a point in time relies on.
func TestRecordsAreOrderedByTheCollectorNotTheExporter(t *testing.T) {
	n := New()
	old := time.Now().Add(-72 * time.Hour)
	future := time.Now().Add(72 * time.Hour)
	first := n.Normalize(decoder.Flow{FlowStart: future}, "netflow9", 1, 1) // exporter says "later"
	second := n.Normalize(decoder.Flow{FlowStart: old}, "netflow9", 1, 1)   // exporter says "earlier"
	if second.FlowStart.Before(first.FlowStart) {
		t.Errorf("arrival order was not preserved: %v then %v", first.FlowStart, second.FlowStart)
	}
}
