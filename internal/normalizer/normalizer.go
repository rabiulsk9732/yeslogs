// Package normalizer converts protocol-agnostic decoder.Flow values into the
// canonical FlowRecord that the rest of the pipeline (rules, writer) operates
// on, attaching the resolved ISP/device identity and the flow-type label.
package normalizer

import (
	"net"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

// FlowRecord is the canonical, enriched representation of a single flow. It maps
// 1:1 to a row in the ClickHouse flow_logs table.
type FlowRecord struct {
	ISPID    uint32
	DeviceID uint32

	SrcIP   net.IP
	SrcPort uint16
	DstIP   net.IP
	DstPort uint16

	NatPublicIP   net.IP
	NatPublicPort uint16

	Protocol uint8
	Bytes    uint64
	Packets  uint64

	FlowStart time.Time
	FlowEnd   time.Time

	FlowType   string
	ExporterIP net.IP
}

// Normalizer maps decoded flows to FlowRecords. It is stateless; the ISP/device
// identity is resolved per packet (from the device registry) and passed in.
type Normalizer struct{}

// New returns a Normalizer.
func New() *Normalizer { return &Normalizer{} }

// Normalize maps a decoder.Flow to a FlowRecord, tagging it with flowType and
// the supplied ISP/device identity.
func (n *Normalizer) Normalize(f decoder.Flow, flowType string, ispID, deviceID uint32) FlowRecord {
	// Fallback timestamp: some exporters (e.g. iptables/conntrack NAT-event NetFlow)
	// carry the time in an IE the decoder may not map. Without a valid time the row
	// would land in an ancient partition and be silently dropped by the TTL, so we
	// fall back to the collector's receive time.
	start, end := f.FlowStart, f.FlowEnd
	if start.IsZero() {
		start = time.Now()
	}
	if end.IsZero() {
		end = start
	}
	return FlowRecord{
		ISPID:         ispID,
		DeviceID:      deviceID,
		SrcIP:         f.SrcIP,
		SrcPort:       f.SrcPort,
		DstIP:         f.DstIP,
		DstPort:       f.DstPort,
		NatPublicIP:   f.NatPublicIP,
		NatPublicPort: f.NatPublicPort,
		Protocol:      f.Protocol,
		Bytes:         f.Bytes,
		Packets:       f.Packets,
		FlowStart:     start,
		FlowEnd:       end,
		FlowType:      flowType,
		ExporterIP:    f.ExporterIP,
	}
}
