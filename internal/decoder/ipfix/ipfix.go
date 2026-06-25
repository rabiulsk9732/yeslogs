// Package ipfix is a placeholder IPFIX (RFC 7011) decoder.
//
// IPFIX is the IETF standardization of NetFlow v9 and is likewise
// template-based (the version field is 10). v1 validates the message header so
// the port can be bound and traffic observed; record decoding returns
// ErrUnsupported pending template support.
package ipfix

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

const (
	version   = 10
	headerLen = 16
)

var (
	// ErrUnsupported indicates IPFIX record decoding is not implemented yet. It
	// wraps decoder.ErrUnsupported so the pipeline can detect it.
	ErrUnsupported = fmt.Errorf("ipfix: template decoding not implemented in v1: %w", decoder.ErrUnsupported)
	// ErrShortPacket is returned when the datagram cannot hold an IPFIX header.
	ErrShortPacket = errors.New("ipfix: packet shorter than header")
	// ErrWrongVersion is returned when the version field is not 10.
	ErrWrongVersion = errors.New("ipfix: not an IPFIX (v10) packet")
)

// Decoder is a stateless placeholder reserving the shape of the future
// template-aware implementation.
type Decoder struct{}

// New returns an IPFIX placeholder decoder.
func New() *Decoder { return &Decoder{} }

// Kind implements decoder.Decoder.
func (d *Decoder) Kind() string { return "ipfix" }

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
