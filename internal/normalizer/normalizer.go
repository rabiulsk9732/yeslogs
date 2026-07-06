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

// Clock-skew guard bounds: the stored timestamp must always be a sane IST time
// regardless of what the exporter reports. A device with a broken/frozen clock
// (e.g. no NTP after a reboot) would otherwise stamp EVERY flow with a wrong time
// and silently corrupt time-based IPDR lookups. We trust the exporter's flow time
// only when it is plausible relative to the collector's receive time; otherwise we
// fall back to receive time (now, stored as IST). skewPast is generous so genuinely
// long-lived/late-exported flows (NetFlow active-timeout is typically ≤30m) are kept.
const (
	skewPast   = 2 * time.Hour
	skewFuture = 2 * time.Minute
)

func plausible(t, now time.Time) bool {
	return !t.IsZero() && !t.After(now.Add(skewFuture)) && !t.Before(now.Add(-skewPast))
}

// Normalize maps a decoder.Flow to a FlowRecord, tagging it with flowType and
// the supplied ISP/device identity.
func (n *Normalizer) Normalize(f decoder.Flow, flowType string, ispID, deviceID uint32) FlowRecord {
	// Whatever timestamp a device sends, the logged time must be sane IST: use the
	// exporter's flow time only when plausible; otherwise stamp with receive time.
	// This also covers exporters (e.g. iptables NAT-event NetFlow) that carry no
	// mappable time IE — those arrive zero and fall back here too.
	now := time.Now()
	start, end := f.FlowStart, f.FlowEnd
	if !plausible(start, now) {
		start = now
	}
	if end.IsZero() || end.Before(start) || end.After(now.Add(skewFuture)) {
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
