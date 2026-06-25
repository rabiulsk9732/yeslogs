// Package sanitize deterministically rewrites IPv4 addresses in captured
// NetFlow/IPFIX traffic — both the IP/UDP headers and the addresses embedded in
// flow records — so a capture can be shared as a test fixture without exposing
// real customer data. v9/IPFIX rewriting is template-aware; a data packet whose
// template has not been seen is dropped (never emitted) so no real address can
// leak through an undecodable record.
package sanitize

import (
	"crypto/rand"
	"encoding/binary"
	"net"

	"github.com/natflow/natflow-dataplane/internal/pcapio"
)

// ipv4IEs are the NetFlow v9 / IPFIX information elements carrying IPv4
// addresses that we rewrite (source, dest, next-hops, post-NAT addresses).
var ipv4IEs = map[uint16]bool{8: true, 12: true, 15: true, 18: true, 225: true, 226: true}

// ipv6IEs carry IPv6 addresses. v0.4.1 does not rewrite IPv6, so a record
// containing one cannot be safely sanitized — such packets are dropped.
var ipv6IEs = map[uint16]bool{27: true, 28: true, 62: true, 63: true, 281: true, 282: true}

// record-walk outcomes.
const (
	recOK         = iota // fully parsed, all address fields safely rewritten
	recIncomplete        // ran out of bytes mid-record (truncation / trailing padding)
	recUnsafe            // an address field could not be safely rewritten -> drop packet
)

type field struct {
	ie         uint16
	length     uint16
	enterprise bool
}

type tmpl struct {
	fields    []field
	recLen    int
	hasVar    bool
	isOptions bool
}

type tkey struct {
	exporter string
	domain   uint32
	id       uint16
}

// Stats summarizes a sanitization run.
type Stats struct {
	Packets      int
	Kept         int
	Dropped      int
	IPsRewritten int
}

// Sanitizer rewrites IPv4 addresses consistently within a run.
type Sanitizer struct {
	key   []byte
	m     map[[4]byte][4]byte
	v9    map[tkey]*tmpl
	ipfix map[tkey]*tmpl
	Stats Stats
}

// New returns a Sanitizer. If key is nil, a random per-run key is generated so
// the mapping is consistent within the run but not reversible across runs; pass
// a fixed key for reproducible output.
func New(key []byte) *Sanitizer {
	if len(key) == 0 {
		key = make([]byte, 16)
		_, _ = rand.Read(key)
	}
	return &Sanitizer{
		key:   key,
		m:     make(map[[4]byte][4]byte),
		v9:    make(map[tkey]*tmpl),
		ipfix: make(map[tkey]*tmpl),
	}
}

// remap returns the deterministic sanitized form of a 4-byte IPv4 address,
// mapped into 10.0.0.0/8 (RFC1918) and consistent within the run.
func (s *Sanitizer) remap(b [4]byte) [4]byte {
	if v, ok := s.m[b]; ok {
		return v
	}
	// FNV-1a over key||addr.
	var h uint32 = 2166136261
	for _, c := range s.key {
		h = (h ^ uint32(c)) * 16777619
	}
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}
	out := [4]byte{10, byte(h >> 16), byte(h >> 8), byte(h)}
	s.m[b] = out
	return out
}

func (s *Sanitizer) rewriteAt(buf []byte, off int) {
	var b [4]byte
	copy(b[:], buf[off:off+4])
	out := s.remap(b)
	copy(buf[off:off+4], out[:])
	s.Stats.IPsRewritten++
}

// Frame sanitizes one link-layer frame in place, returning false when the frame
// must be dropped. The policy is "drop anything we cannot fully sanitize": a
// non-IPv4 frame (IPv6/ARP/...) is dropped (its addresses are not ones we know
// how to rewrite), and an IPv4/UDP flow datagram is dropped if its payload
// cannot be fully scrubbed (unknown template, options data, odd field, partial
// record). Every kept frame has all IPv4 addresses rewritten.
func (s *Sanitizer) Frame(linkType uint32, frame []byte) bool {
	s.Stats.Packets++
	l3, ihl, isIPv4 := pcapio.ParseIPv4(linkType, frame)
	if !isIPv4 {
		s.Stats.Dropped++ // non-IPv4: cannot guarantee sanitization
		return false
	}

	// If this is a flow UDP datagram, scrub the payload first (may drop).
	if info, isUDP := pcapio.ParseUDP(linkType, frame); isUDP {
		var src [4]byte
		copy(src[:], frame[l3+12:l3+16])
		exporter := net.IP(src[:]).String()
		payload := frame[info.PayloadOff : info.PayloadOff+info.PayloadLen]
		if !s.sanitizePayload(exporter, payload) {
			s.Stats.Dropped++
			return false
		}
		binary.BigEndian.PutUint16(frame[info.UDPOff+6:info.UDPOff+8], 0) // zero UDP checksum (valid for IPv4)
	}

	// Rewrite the IPv4 header addresses (for any IPv4 frame) and fix the checksum.
	s.rewriteAt(frame, l3+12)
	s.rewriteAt(frame, l3+16)
	cs := pcapio.IPv4Checksum(frame[l3 : l3+ihl])
	binary.BigEndian.PutUint16(frame[l3+10:l3+12], cs)

	s.Stats.Kept++
	return true
}

func (s *Sanitizer) sanitizePayload(exporter string, p []byte) bool {
	if len(p) < 4 {
		return false // not a flow datagram we can validate -> drop
	}
	switch binary.BigEndian.Uint16(p[0:2]) {
	case 5:
		return s.sanitizeV5(p)
	case 9:
		return s.sanitizeV9(exporter, p)
	case 10:
		return s.sanitizeIPFIX(exporter, p)
	default:
		return false // not NetFlow/IPFIX: its payload may hold IPs we can't find -> drop
	}
}

func (s *Sanitizer) sanitizeV5(p []byte) bool {
	const hdr, rec = 24, 48
	if len(p) < hdr {
		return false
	}
	body := len(p) - hdr
	if body%rec != 0 {
		return false // partial trailing record could carry address bytes -> drop
	}
	for pos := hdr; pos+rec <= len(p); pos += rec {
		s.rewriteAt(p, pos)   // srcaddr
		s.rewriteAt(p, pos+4) // dstaddr
		s.rewriteAt(p, pos+8) // nexthop
	}
	return true
}

func (s *Sanitizer) sanitizeV9(exporter string, p []byte) bool {
	if len(p) < 20 {
		return true
	}
	domain := binary.BigEndian.Uint32(p[16:20])
	off := 20
	for off+4 <= len(p) {
		fsID := binary.BigEndian.Uint16(p[off : off+2])
		fsLen := int(binary.BigEndian.Uint16(p[off+2 : off+4]))
		if fsLen < 4 || off+fsLen > len(p) {
			return false // malformed/truncated: drop rather than risk a leak
		}
		body := p[off+4 : off+fsLen]
		switch {
		case fsID == 0:
			s.parseV9Templates(exporter, domain, body)
		case fsID == 1:
			if len(body) >= 2 {
				s.v9[tkey{exporter, domain, binary.BigEndian.Uint16(body[0:2])}] = &tmpl{isOptions: true}
			}
		case fsID >= 256:
			t := s.v9[tkey{exporter, domain, fsID}]
			if t == nil {
				return false // unknown template: cannot safely sanitize
			}
			if t.isOptions {
				return false // options data may carry unmapped addresses -> drop
			}
			if !s.rewriteFixedRecords(body, t) {
				return false
			}
		}
		off += fsLen
	}
	return true
}

func (s *Sanitizer) parseV9Templates(exporter string, domain uint32, body []byte) {
	p := 0
	for p+4 <= len(body) {
		tid := binary.BigEndian.Uint16(body[p : p+2])
		fc := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		p += 4
		if fc == 0 || p+fc*4 > len(body) {
			break
		}
		fields := make([]field, fc)
		recLen := 0
		for i := 0; i < fc; i++ {
			ie := binary.BigEndian.Uint16(body[p : p+2])
			fl := binary.BigEndian.Uint16(body[p+2 : p+4])
			p += 4
			fields[i] = field{ie: ie, length: fl}
			recLen += int(fl)
		}
		s.v9[tkey{exporter, domain, tid}] = &tmpl{fields: fields, recLen: recLen}
	}
}

func (s *Sanitizer) sanitizeIPFIX(exporter string, p []byte) bool {
	if len(p) < 16 {
		return true
	}
	end := len(p)
	if msgLen := int(binary.BigEndian.Uint16(p[2:4])); msgLen >= 16 && msgLen < end {
		end = msgLen
	}
	domain := binary.BigEndian.Uint32(p[12:16])
	off := 16
	for off+4 <= end {
		setID := binary.BigEndian.Uint16(p[off : off+2])
		setLen := int(binary.BigEndian.Uint16(p[off+2 : off+4]))
		if setLen < 4 || off+setLen > end {
			return false
		}
		body := p[off+4 : off+setLen]
		switch {
		case setID == 2:
			s.parseIPFIXTemplates(exporter, domain, body)
		case setID == 3:
			if len(body) >= 2 {
				s.ipfix[tkey{exporter, domain, binary.BigEndian.Uint16(body[0:2])}] = &tmpl{isOptions: true}
			}
		case setID >= 256:
			t := s.ipfix[tkey{exporter, domain, setID}]
			if t == nil {
				return false
			}
			if t.isOptions {
				return false // options data may carry unmapped addresses -> drop
			}
			if !s.rewriteVarRecords(body, t) {
				return false
			}
		}
		off += setLen
	}
	return true
}

func (s *Sanitizer) parseIPFIXTemplates(exporter string, domain uint32, body []byte) {
	p := 0
	for p+4 <= len(body) {
		tid := binary.BigEndian.Uint16(body[p : p+2])
		fc := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		p += 4
		if tid == 0 {
			break
		}
		if fc == 0 {
			continue
		}
		fields := make([]field, 0, fc)
		recLen, hasVar, ok := 0, false, true
		for i := 0; i < fc; i++ {
			if p+4 > len(body) {
				ok = false
				break
			}
			ie := binary.BigEndian.Uint16(body[p : p+2])
			fl := binary.BigEndian.Uint16(body[p+2 : p+4])
			p += 4
			ent := ie&0x8000 != 0
			if ent {
				if p+4 > len(body) {
					ok = false
					break
				}
				p += 4
			}
			if fl == 0xffff {
				hasVar = true
			} else {
				recLen += int(fl)
			}
			fields = append(fields, field{ie: ie & 0x7fff, length: fl, enterprise: ent})
		}
		if !ok {
			break
		}
		s.ipfix[tkey{exporter, domain, tid}] = &tmpl{fields: fields, recLen: recLen, hasVar: hasVar}
	}
}

// rewriteFixedRecords rewrites IPv4 fields in fixed-length records, returning
// false (drop the packet) on a partial trailing record or an unsafe field.
func (s *Sanitizer) rewriteFixedRecords(body []byte, t *tmpl) bool {
	if t.recLen <= 0 {
		return len(body) == 0
	}
	if len(body)%t.recLen != 0 {
		return false // partial trailing record -> drop
	}
	for pos := 0; pos+t.recLen <= len(body); pos += t.recLen {
		if _, st := s.rewriteRecordFields(body[pos:pos+t.recLen], t.fields); st != recOK {
			return false
		}
	}
	return true
}

// rewriteVarRecords rewrites IPv4 fields in (possibly variable-length) records,
// returning false (drop) on an unsafe field or non-zero trailing bytes.
func (s *Sanitizer) rewriteVarRecords(body []byte, t *tmpl) bool {
	if !t.hasVar {
		return s.rewriteFixedRecords(body, t)
	}
	for pos := 0; pos < len(body); {
		n, st := s.rewriteRecordFields(body[pos:], t.fields)
		switch st {
		case recUnsafe:
			return false
		case recIncomplete:
			for _, b := range body[pos:] { // remaining must be zero padding
				if b != 0 {
					return false
				}
			}
			return true
		}
		if n == 0 {
			return false // no progress -> avoid infinite loop
		}
		pos += n
	}
	return true
}

// rewriteRecordFields walks one record, rewriting IPv4-IE values. It returns the
// bytes consumed and a status: recOK (done), recIncomplete (ran out of bytes),
// or recUnsafe (an address field we cannot safely rewrite — caller must drop).
func (s *Sanitizer) rewriteRecordFields(rec []byte, fields []field) (int, int) {
	pos := 0
	for _, f := range fields {
		flen := int(f.length)
		if f.length == 0xffff {
			if pos >= len(rec) {
				return pos, recIncomplete
			}
			flen = int(rec[pos])
			pos++
			if flen == 255 {
				if pos+2 > len(rec) {
					return pos, recIncomplete
				}
				flen = int(binary.BigEndian.Uint16(rec[pos : pos+2]))
				pos += 2
			}
		}
		if pos+flen > len(rec) {
			return pos, recIncomplete
		}
		if !f.enterprise {
			if ipv6IEs[f.ie] {
				return pos, recUnsafe // IPv6 address: not rewritten in v0.4.1 -> drop
			}
			if ipv4IEs[f.ie] {
				if flen != 4 {
					return pos, recUnsafe // odd-length IPv4 IE -> drop
				}
				s.rewriteAt(rec, pos)
			}
		}
		pos += flen
	}
	return pos, recOK
}
