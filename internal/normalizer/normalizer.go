// Package normalizer converts protocol-agnostic decoder.Flow values into the
// canonical FlowRecord the rest of the pipeline (rules, writer) operates on,
// attaching ISP/device identity and the flow-type label.
package normalizer

import (
	"net"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

// FlowRecord is the canonical, enriched representation of a single flow. It
// maps 1:1 to a row in the ClickHouse flow_logs table.
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

// Normalizer enriches decoded flows with static server identity.
type Normalizer struct {
	ispID    uint32
	deviceID uint32
}

// New returns a Normalizer that stamps every record with ispID and the default
// deviceID (used when the protocol carries no device identifier).
func New(ispID, deviceID uint32) *Normalizer {
	return &Normalizer{ispID: ispID, deviceID: deviceID}
}

// Normalize maps a decoder.Flow to a FlowRecord, tagging it with flowType.
func (n *Normalizer) Normalize(f decoder.Flow, flowType string) FlowRecord {
	return FlowRecord{
		ISPID:         n.ispID,
		DeviceID:      n.deviceID,
		SrcIP:         f.SrcIP,
		SrcPort:       f.SrcPort,
		DstIP:         f.DstIP,
		DstPort:       f.DstPort,
		NatPublicIP:   f.NatPublicIP,
		NatPublicPort: f.NatPublicPort,
		Protocol:      f.Protocol,
		Bytes:         f.Bytes,
		Packets:       f.Packets,
		FlowStart:     f.FlowStart,
		FlowEnd:       f.FlowEnd,
		FlowType:      flowType,
		ExporterIP:    f.ExporterIP,
	}
}
