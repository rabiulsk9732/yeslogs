// Package netflow5 implements a NetFlow version 5 decoder.
//
// NetFlow v5 is a fixed-format, template-free protocol: a datagram is a 24-byte
// header followed by up to 30 fixed 48-byte flow records. The decoder is
// stateless and safe for concurrent use.
package netflow5

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

const (
	version    = 5
	headerLen  = 24
	recordLen  = 48
	maxRecords = 30 // protocol maximum records per datagram
)

var (
	// ErrShortPacket is returned when payload is too small to hold a header.
	ErrShortPacket = errors.New("netflow5: packet shorter than header")
	// ErrWrongVersion is returned when the version field is not 5.
	ErrWrongVersion = errors.New("netflow5: not a version 5 packet")
)

// Decoder decodes NetFlow v5 datagrams. It holds no state and is safe for
// concurrent use by multiple goroutines.
type Decoder struct{}

// New returns a ready-to-use NetFlow v5 decoder.
func New() *Decoder { return &Decoder{} }

// Kind implements decoder.Decoder.
func (d *Decoder) Kind() string { return "netflow5" }

// Decode implements decoder.Decoder.
func (d *Decoder) Decode(dst []decoder.Flow, payload []byte, exporter net.IP) ([]decoder.Flow, error) {
	if len(payload) < headerLen {
		return dst, ErrShortPacket
	}
	if v := binary.BigEndian.Uint16(payload[0:2]); v != version {
		return dst, fmt.Errorf("%w: got %d", ErrWrongVersion, v)
	}

	count := int(binary.BigEndian.Uint16(payload[2:4]))
	if count > maxRecords {
		count = maxRecords
	}
	// Clamp to what the datagram actually contains so we never read past its end.
	if avail := (len(payload) - headerLen) / recordLen; count > avail {
		count = avail
	}
	if count == 0 {
		return dst, nil
	}

	sysUptimeMS := uint64(binary.BigEndian.Uint32(payload[4:8]))
	unixSecs := uint64(binary.BigEndian.Uint32(payload[8:12]))
	unixNsecs := uint64(binary.BigEndian.Uint32(payload[12:16]))

	// flow first/last are exporter-uptime millisecond timestamps. Convert them
	// to absolute wall-clock time via the exporter's boot instant:
	//   bootMS    = exportTime - sysUptime
	//   flowStart = bootMS + first
	exportMS := unixSecs*1000 + unixNsecs/1_000_000
	bootMS := int64(exportMS) - int64(sysUptimeMS)

	// Copy the exporter IP once; the caller may reuse its backing array.
	exp := cloneIP4(exporter)

	for i := 0; i < count; i++ {
		off := headerLen + i*recordLen
		rec := payload[off : off+recordLen]

		first := uint64(binary.BigEndian.Uint32(rec[24:28]))
		last := uint64(binary.BigEndian.Uint32(rec[28:32]))

		dst = append(dst, decoder.Flow{
			SrcIP:      net.IP{rec[0], rec[1], rec[2], rec[3]},
			DstIP:      net.IP{rec[4], rec[5], rec[6], rec[7]},
			SrcPort:    binary.BigEndian.Uint16(rec[32:34]),
			DstPort:    binary.BigEndian.Uint16(rec[34:36]),
			Packets:    uint64(binary.BigEndian.Uint32(rec[16:20])),
			Bytes:      uint64(binary.BigEndian.Uint32(rec[20:24])),
			TCPFlags:   rec[37],
			Protocol:   rec[38],
			TOS:        rec[39],
			FlowStart:  msToTime(bootMS + int64(first)),
			FlowEnd:    msToTime(bootMS + int64(last)),
			ExporterIP: exp,
		})
	}
	return dst, nil
}

func msToTime(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

func cloneIP4(ip net.IP) net.IP {
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	out := make(net.IP, net.IPv4len)
	copy(out, v4)
	return out
}
