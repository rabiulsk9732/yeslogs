package normalizer

import (
	"net"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

func TestNormalize(t *testing.T) {
	n := New()
	start := time.Unix(1_600_000_000, 0).UTC()
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
	if !r.FlowEnd.After(r.FlowStart) {
		t.Error("flowEnd should be after flowStart")
	}
}
