// Package decoder defines the common contract that every flow-protocol decoder
// (NetFlow v5/v9, IPFIX) implements, plus the protocol-agnostic Flow value that
// decoders emit before normalization.
package decoder

import (
	"errors"
	"net"
	"time"
)

// ErrUnsupported is wrapped by decoders that recognize a packet's protocol but
// cannot yet decode its records (e.g. the v1 NetFlow v9 / IPFIX placeholders).
// The pipeline uses errors.Is to count these separately from malformed packets.
var ErrUnsupported = errors.New("decoder: protocol decoding not implemented")

// Flow is the raw, protocol-agnostic representation of a single flow record as
// produced by a Decoder. It stays close to the wire format; enrichment
// (ISP/device identity, flow typing) happens later in the normalizer.
type Flow struct {
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16

	Protocol uint8
	TOS      uint8
	TCPFlags uint8

	Bytes   uint64
	Packets uint64

	FlowStart time.Time
	FlowEnd   time.Time

	// NAT translation fields. NetFlow v5 never carries these; NetFlow v9 and
	// IPFIX NAT-event templates may. The zero value means "not translated".
	NatPublicIP   net.IP
	NatPublicPort uint16

	// ExporterIP is the source IP of the UDP datagram that carried this flow.
	ExporterIP net.IP
}

// Decoder parses a single UDP payload into zero or more Flow records.
//
// Implementations must be safe for concurrent use: the receiver calls Decode
// from multiple worker goroutines simultaneously. Decoders therefore must not
// keep mutable state across calls without their own synchronization (a
// template-based decoder must guard its template cache).
type Decoder interface {
	// Kind returns a stable, lower-case protocol label, e.g. "netflow5". It is
	// copied verbatim into FlowRecord.FlowType.
	Kind() string

	// Decode parses payload (the bytes of one UDP datagram from exporter) and
	// appends the decoded flows to dst, returning the extended slice. Callers
	// reuse dst across packets to avoid per-packet allocation, so
	// implementations must only append, never assume dst is empty.
	Decode(dst []Flow, payload []byte, exporter net.IP) ([]Flow, error)
}
