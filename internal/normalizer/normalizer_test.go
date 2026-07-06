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
	if !r.FlowStart.Equal(start) {
		t.Errorf("plausible FlowStart should be preserved: got %v want %v", r.FlowStart, start)
	}
	if !r.FlowEnd.After(r.FlowStart) {
		t.Error("flowEnd should be after flowStart")
	}
}

// A device with a broken/frozen clock (or no time IE) must not corrupt stored
// timestamps — they clamp to the collector's receive time.
func TestNormalizeClockSkew(t *testing.T) {
	n := New()
	now := time.Now()
	cases := []struct {
		name      string
		start     time.Time
		wantClamp bool
	}{
		{"zero", time.Time{}, true},
		{"frozen-12h-ago", now.Add(-12 * time.Hour), true}, // the Wadhai case
		{"far-future", now.Add(10 * time.Minute), true},    // clock ahead
		{"recent-ok", now.Add(-90 * time.Second), false},
		{"within-active-timeout", now.Add(-25 * time.Minute), false},
	}
	for _, c := range cases {
		r := n.Normalize(decoder.Flow{FlowStart: c.start}, "netflow9", 1, 1)
		clamped := r.FlowStart.Sub(now) < 2*time.Second && r.FlowStart.Sub(now) > -2*time.Second
		if c.wantClamp && !clamped {
			t.Errorf("%s: expected clamp to receive-time, got %v", c.name, r.FlowStart)
		}
		if !c.wantClamp && !r.FlowStart.Equal(c.start) {
			t.Errorf("%s: expected preserved %v, got %v", c.name, c.start, r.FlowStart)
		}
		if r.FlowEnd.Before(r.FlowStart) {
			t.Errorf("%s: FlowEnd before FlowStart", c.name)
		}
	}
}
