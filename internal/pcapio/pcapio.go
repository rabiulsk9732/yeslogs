// Package pcapio reads and writes classic libpcap files and locates the UDP
// payload inside a link-layer frame, with no cgo/libpcap dependency. It supports
// the link types tcpdump commonly produces (Ethernet, raw IP, Linux SLL/SLL2,
// BSD loopback).
package pcapio

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Link-layer types (libpcap DLT/LINKTYPE values) we understand.
const (
	LinkEthernet = 1
	LinkRaw      = 101
	LinkRawAlt   = 12 // some DLT_RAW captures
	LinkNull     = 0  // BSD loopback (host-endian family)
	LinkLoop     = 108
	LinkSLL      = 113
	LinkSLL2     = 276
)

const globalHeaderLen = 24

// Packet is one captured frame.
type Packet struct {
	Time    time.Time
	OrigLen int    // original on-wire length (may exceed len(Data) if snaplen-truncated)
	Data    []byte // captured link-layer frame
}

// Reader reads packets from a libpcap stream.
type Reader struct {
	r        io.Reader
	bo       binary.ByteOrder
	nano     bool
	linkType uint32
	hdr      [16]byte
}

// NewReader parses the global header and returns a Reader.
func NewReader(r io.Reader) (*Reader, error) {
	var gh [globalHeaderLen]byte
	if _, err := io.ReadFull(r, gh[:]); err != nil {
		return nil, fmt.Errorf("read pcap global header: %w", err)
	}
	magic := binary.LittleEndian.Uint32(gh[0:4])
	rd := &Reader{r: r}
	switch magic {
	case 0xa1b2c3d4:
		rd.bo = binary.LittleEndian
	case 0xd4c3b2a1:
		rd.bo = binary.BigEndian
	case 0xa1b23c4d:
		rd.bo, rd.nano = binary.LittleEndian, true
	case 0x4d3cb2a1:
		rd.bo, rd.nano = binary.BigEndian, true
	default:
		return nil, fmt.Errorf("not a pcap file (magic %#x)", magic)
	}
	rd.linkType = rd.bo.Uint32(gh[20:24])
	return rd, nil
}

// LinkType returns the capture's link-layer type.
func (r *Reader) LinkType() uint32 { return r.linkType }

// Next returns the next packet, or io.EOF at the end.
func (r *Reader) Next() (Packet, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return Packet{}, err
	}
	tsSec := r.bo.Uint32(r.hdr[0:4])
	tsFrac := r.bo.Uint32(r.hdr[4:8])
	inclLen := r.bo.Uint32(r.hdr[8:12])
	origLen := r.bo.Uint32(r.hdr[12:16])
	if inclLen > 1<<24 {
		return Packet{}, fmt.Errorf("implausible packet length %d", inclLen)
	}
	data := make([]byte, inclLen)
	if _, err := io.ReadFull(r.r, data); err != nil {
		return Packet{}, fmt.Errorf("short packet body: %w", err)
	}
	var t time.Time
	if r.nano {
		t = time.Unix(int64(tsSec), int64(tsFrac)).UTC()
	} else {
		t = time.Unix(int64(tsSec), int64(tsFrac)*1000).UTC()
	}
	return Packet{Time: t, OrigLen: int(origLen), Data: data}, nil
}

// Writer writes a libpcap stream (little-endian, microsecond resolution).
type Writer struct {
	w io.Writer
}

// NewWriter writes the global header and returns a Writer.
func NewWriter(w io.Writer, linkType uint32) (*Writer, error) {
	var gh [globalHeaderLen]byte
	binary.LittleEndian.PutUint32(gh[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(gh[4:6], 2) // version major
	binary.LittleEndian.PutUint16(gh[6:8], 4) // version minor
	binary.LittleEndian.PutUint32(gh[16:20], 262144)
	binary.LittleEndian.PutUint32(gh[20:24], linkType)
	if _, err := w.Write(gh[:]); err != nil {
		return nil, err
	}
	return &Writer{w: w}, nil
}

// Write appends a packet.
func (w *Writer) Write(p Packet) error {
	orig := len(p.Data)
	if p.OrigLen > orig {
		orig = p.OrigLen
	}
	var h [16]byte
	binary.LittleEndian.PutUint32(h[0:4], uint32(p.Time.Unix()))
	binary.LittleEndian.PutUint32(h[4:8], uint32(p.Time.Nanosecond()/1000))
	binary.LittleEndian.PutUint32(h[8:12], uint32(len(p.Data)))
	binary.LittleEndian.PutUint32(h[12:16], uint32(orig))
	if _, err := w.w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.w.Write(p.Data)
	return err
}

// UDPInfo locates the IPv4/UDP structure within a link-layer frame.
type UDPInfo struct {
	L3Off      int // offset of the IPv4 header
	IHL        int // IPv4 header length in bytes
	UDPOff     int // offset of the UDP header
	PayloadOff int // offset of the UDP payload
	PayloadLen int // length of the UDP payload
	SrcPort    uint16
	DstPort    uint16
}

// l3Offset returns the byte offset of the L3 (IP) header for a link type, and
// whether the frame is plausibly IPv4. Returns ok=false for unsupported links.
func l3Offset(linkType uint32, frame []byte) (int, bool) {
	switch linkType {
	case LinkEthernet:
		if len(frame) < 14 {
			return 0, false
		}
		off := 14
		et := binary.BigEndian.Uint16(frame[12:14])
		for et == 0x8100 || et == 0x88a8 { // VLAN tags
			if len(frame) < off+4 {
				return 0, false
			}
			et = binary.BigEndian.Uint16(frame[off+2 : off+4])
			off += 4
		}
		if et != 0x0800 {
			return 0, false
		}
		return off, true
	case LinkRaw, LinkRawAlt:
		return 0, true
	case LinkSLL:
		if len(frame) < 16 {
			return 0, false
		}
		if binary.BigEndian.Uint16(frame[14:16]) != 0x0800 {
			return 0, false
		}
		return 16, true
	case LinkSLL2:
		if len(frame) < 20 {
			return 0, false
		}
		if binary.BigEndian.Uint16(frame[0:2]) != 0x0800 {
			return 0, false
		}
		return 20, true
	case LinkNull, LinkLoop:
		if len(frame) < 4 {
			return 0, false
		}
		return 4, true // 4-byte address-family header; assume IPv4 family
	default:
		return 0, false
	}
}

// ParseIPv4 returns the IPv4 header offset and length for any IPv4 frame
// (regardless of L4 protocol). ok=false for non-IPv4 or malformed frames.
func ParseIPv4(linkType uint32, frame []byte) (l3Off, ihl int, ok bool) {
	l3, ok := l3Offset(linkType, frame)
	if !ok || len(frame) < l3+20 {
		return 0, 0, false
	}
	if frame[l3]>>4 != 4 {
		return 0, 0, false
	}
	ihl = int(frame[l3]&0x0f) * 4
	if ihl < 20 || len(frame) < l3+ihl {
		return 0, 0, false
	}
	return l3, ihl, true
}

// ParseUDP locates the IPv4/UDP payload in frame. It returns ok=false when the
// frame is not IPv4/UDP or is malformed/unsupported.
func ParseUDP(linkType uint32, frame []byte) (UDPInfo, bool) {
	l3, ihl, ok := ParseIPv4(linkType, frame)
	if !ok || len(frame) < l3+ihl+8 {
		return UDPInfo{}, false
	}
	if frame[l3+9] != 17 { // protocol != UDP
		return UDPInfo{}, false
	}
	udp := l3 + ihl
	udpLen := int(binary.BigEndian.Uint16(frame[udp+4 : udp+6]))
	payloadOff := udp + 8
	payloadLen := udpLen - 8
	if payloadLen < 0 || payloadOff+payloadLen > len(frame) {
		payloadLen = len(frame) - payloadOff // tolerate truncation/snaplen
	}
	return UDPInfo{
		L3Off:      l3,
		IHL:        ihl,
		UDPOff:     udp,
		PayloadOff: payloadOff,
		PayloadLen: payloadLen,
		SrcPort:    binary.BigEndian.Uint16(frame[udp : udp+2]),
		DstPort:    binary.BigEndian.Uint16(frame[udp+2 : udp+4]),
	}, true
}

// EthernetIPv4UDP builds a synthetic Ethernet/IPv4/UDP frame carrying payload,
// for generating fixtures and tests. srcIP/dstIP are 4-byte IPv4 addresses.
func EthernetIPv4UDP(srcIP, dstIP [4]byte, srcPort, dstPort uint16, payload []byte) []byte {
	const eth, ip, udp = 14, 20, 8
	frame := make([]byte, eth+ip+udp+len(payload))
	// Ethernet: dummy MACs, ethertype IPv4.
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	// IPv4 header.
	h := frame[eth:]
	h[0] = 0x45 // version 4, IHL 5
	binary.BigEndian.PutUint16(h[2:4], uint16(ip+udp+len(payload)))
	h[8] = 64 // TTL
	h[9] = 17 // UDP
	copy(h[12:16], srcIP[:])
	copy(h[16:20], dstIP[:])
	binary.BigEndian.PutUint16(h[10:12], IPv4Checksum(h[:ip]))
	// UDP header (checksum 0 = valid for IPv4).
	u := frame[eth+ip:]
	binary.BigEndian.PutUint16(u[0:2], srcPort)
	binary.BigEndian.PutUint16(u[2:4], dstPort)
	binary.BigEndian.PutUint16(u[4:6], uint16(udp+len(payload)))
	copy(frame[eth+ip+udp:], payload)
	return frame
}

// IPv4Checksum computes the IPv4 header checksum over hdr (with the checksum
// field treated as zero).
func IPv4Checksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(hdr); i += 2 {
		if i == 10 { // skip the checksum field
			continue
		}
		sum += uint32(binary.BigEndian.Uint16(hdr[i : i+2]))
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
