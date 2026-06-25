// Package netflow9 is a placeholder NetFlow v9 decoder.
//
// NetFlow v9 is template-based: exporters periodically send template FlowSets
// that define the layout of subsequent data FlowSets. A full implementation
// must cache templates per (exporter, source-id, template-id) and decode data
// records against them. v1 ships header validation only, so the port can be
// bound and traffic observed; data decoding returns ErrUnsupported.
package netflow9

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

const (
	version   = 9
	headerLen = 20
)

var (
	// ErrUnsupported indicates v9 data decoding is not implemented yet. It
	// wraps decoder.ErrUnsupported so the pipeline can detect it.
	ErrUnsupported = fmt.Errorf("netflow9: template decoding not implemented in v1: %w", decoder.ErrUnsupported)
	// ErrShortPacket is returned when the datagram cannot hold a v9 header.
	ErrShortPacket = errors.New("netflow9: packet shorter than header")
	// ErrWrongVersion is returned when the version field is not 9.
	ErrWrongVersion = errors.New("netflow9: not a version 9 packet")
)

// Decoder is a stateless placeholder. The real implementation will hold a
// template cache guarded by a mutex; this struct reserves that shape.
type Decoder struct{}

// New returns a NetFlow v9 placeholder decoder.
func New() *Decoder { return &Decoder{} }

// Kind implements decoder.Decoder.
func (d *Decoder) Kind() string { return "netflow9" }

// Decode validates the header and returns ErrUnsupported. It never appends.
func (d *Decoder) Decode(dst []decoder.Flow, payload []byte, _ net.IP) ([]decoder.Flow, error) {
	if len(payload) < headerLen {
		return dst, ErrShortPacket
	}
	if v := binary.BigEndian.Uint16(payload[0:2]); v != version {
		return dst, ErrWrongVersion
	}
	return dst, ErrUnsupported
}
