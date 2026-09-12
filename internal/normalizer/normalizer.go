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
	NatDestIP     net.IP
	NatDestPort   uint16
	// NatEvent is IE 230: 1 = allocation, 2 = release, 0 = not reported.
	NatEvent uint8
	// Username is IE 371, the subscriber identity as the exporter knows it.
	Username string

	Protocol uint8
	Bytes    uint64
	Packets  uint64

	FlowStart time.Time
	FlowEnd   time.Time
	// ExporterFlow* preserve the timestamps reported on the wire while
	// FlowStart/FlowEnd remain the trusted collector timeline for compatibility.
	ExporterFlowStart time.Time
	ExporterFlowEnd   time.Time
	CollectorReceived time.Time

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
	// Every record is stamped with the COLLECTOR's clock, never the exporter's.
	//
	// This is a deliberate policy, not a fallback. Exporter clocks are not
	// trustworthy — one on this fleet runs 38.5 hours behind — and a store whose
	// records carry a mixture of good and bad clocks cannot produce a defensible
	// timeline at all: two records an hour apart on the wire can land days apart
	// in the table, and nothing downstream can tell which is which. One clock,
	// NTP-synced here, keeps every record on the fleet comparable with every
	// other, which is what a lawful request about a point in time needs.
	//
	// Records arrive within seconds of the event for NAT-event exports, so the
	// receive time IS the event time to the precision anyone can act on.
	now := time.Now()
	start, end := now, now
	return FlowRecord{
		ISPID:             ispID,
		DeviceID:          deviceID,
		SrcIP:             f.SrcIP,
		SrcPort:           f.SrcPort,
		DstIP:             f.DstIP,
		DstPort:           f.DstPort,
		NatPublicIP:       f.NatPublicIP,
		NatPublicPort:     f.NatPublicPort,
		NatDestIP:         f.NatDestIP,
		NatDestPort:       f.NatDestPort,
		NatEvent:          f.NatEvent,
		Username:          f.Username,
		Protocol:          f.Protocol,
		Bytes:             f.Bytes,
		Packets:           f.Packets,
		FlowStart:         start,
		FlowEnd:           end,
		ExporterFlowStart: f.FlowStart,
		ExporterFlowEnd:   f.FlowEnd,
		CollectorReceived: now,
		FlowType:          flowType,
		ExporterIP:        f.ExporterIP,
	}
}
