// Package ipfix implements an IPFIX (RFC 7011) decoder.
//
// IPFIX is the IETF standardization of NetFlow v9 (version field 10). It is
// likewise template-based but adds: Set IDs 2 (Template) and 3 (Options
// Template) instead of v9's 0/1, enterprise-specific information elements (high
// bit of the element ID set, followed by a 4-byte enterprise number),
// variable-length fields (declared length 0xFFFF, with the real length encoded
// inline in each record), and absolute timestamp elements. The decoder caches
// templates per (exporter, observation-domain, template-id), decodes Data Sets
// against them, safely skips enterprise fields and Options data, and is safe
// for concurrent use.
package ipfix

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

const (
	version   = 10
	headerLen = 16

	setTemplate        = 2 // Template Set
	setOptionsTemplate = 3 // Options Template Set
	setDataMin         = 256

	enterpriseBit = 0x8000
	varLen        = 0xFFFF

	templateTTL         = 30 * time.Minute
	sweepEvery          = time.Minute
	maxTemplates        = 1 << 16
	maxRecordsPerPacket = 8192 // bound flow-amplification from one datagram
)

var (
	// ErrShortPacket is returned when the datagram cannot hold an IPFIX header.
	ErrShortPacket = errors.New("ipfix: packet shorter than header")
	// ErrWrongVersion is returned when the version field is not 10.
	ErrWrongVersion = errors.New("ipfix: not an IPFIX (v10) packet")
)

// IPFIX information element IDs we map onto decoder.Flow.
const (
	eOCTETS        = 1
	ePKTS          = 2
	ePROTOCOL      = 4
	eTOS           = 5
	eTCP_FLAGS     = 6
	eSRC_PORT      = 7
	eSRC_IPV4      = 8
	eDST_PORT      = 11
	eDST_IPV4      = 12
	ePOST_OCTETS   = 23
	eSRC_IPV6      = 27
	eDST_IPV6      = 28
	eFLOW_START_S  = 150 // flowStartSeconds
	eFLOW_END_S    = 151 // flowEndSeconds
	eFLOW_START_MS = 152 // flowStartMilliseconds
	eFLOW_END_MS   = 153 // flowEndMilliseconds
	eNAT_SRC_IPV4  = 225 // postNATSourceIPv4Address
	eNAT_SRC_PORT  = 227 // postNAPTSourceTransportPort
)

type field struct {
	ie         uint16
	length     uint16 // varLen (0xFFFF) means variable-length
	enterprise bool
}

type template struct {
	fields    []field
	recordLen int // valid only when !hasVarlen
	hasVarlen bool
	isOptions bool
	lastSeen  atomic.Int64
}

type templateKey struct {
	exporter  string
	obsDomain uint32
	id        uint16
}

// Decoder decodes IPFIX messages, caching templates across messages.
type Decoder struct {
	mu        sync.RWMutex
	templates map[templateKey]*template
	lastSweep int64

	templatesReceived prometheus.Counter
	unknownTemplate   prometheus.Counter
}

// New returns an IPFIX decoder. The two counters are optional (may be nil).
func New(templatesReceived, unknownTemplate prometheus.Counter) *Decoder {
	return &Decoder{
		templates:         make(map[templateKey]*template),
		templatesReceived: templatesReceived,
		unknownTemplate:   unknownTemplate,
	}
}

// Kind implements decoder.Decoder.
func (d *Decoder) Kind() string { return "ipfix" }

// Decode implements decoder.Decoder.
func (d *Decoder) Decode(dst []decoder.Flow, payload []byte, exporter net.IP) ([]decoder.Flow, error) {
	if len(payload) < headerLen {
		return dst, ErrShortPacket
	}
	if binary.BigEndian.Uint16(payload[0:2]) != version {
		return dst, ErrWrongVersion
	}

	// The message length field bounds parsing; clamp to the datagram size.
	end := len(payload)
	if msgLen := int(binary.BigEndian.Uint16(payload[2:4])); msgLen >= headerLen && msgLen < end {
		end = msgLen
	}
	exportTime := time.Unix(int64(binary.BigEndian.Uint32(payload[4:8])), 0).UTC()
	obsDomain := binary.BigEndian.Uint32(payload[12:16])
	exp := exporter.String()

	budget := maxRecordsPerPacket
	off := headerLen
	for off+4 <= end {
		setID := binary.BigEndian.Uint16(payload[off : off+2])
		setLen := int(binary.BigEndian.Uint16(payload[off+2 : off+4]))
		if setLen < 4 || off+setLen > end {
			break
		}
		body := payload[off+4 : off+setLen]

		switch {
		case setID == setTemplate:
			d.parseTemplates(exp, obsDomain, body)
		case setID == setOptionsTemplate:
			d.parseOptionsTemplates(exp, obsDomain, body)
		case setID >= setDataMin:
			before := len(dst)
			dst = d.decodeData(dst, exp, obsDomain, setID, body, exportTime, exporter, budget)
			if budget -= len(dst) - before; budget <= 0 {
				return dst, nil
			}
		default:
			// Set IDs 0, 1, 4..255 are reserved; ignore.
		}
		off += setLen
	}
	return dst, nil
}

// parseFieldSpecs reads count field specifiers starting at *p, handling the
// enterprise bit (which adds a 4-byte enterprise number). It advances *p.
func parseFieldSpecs(body []byte, p *int, count int) (fields []field, recLen int, hasVar bool, ok bool) {
	// Cap the initial capacity to what the body can actually hold (each spec is
	// at least 4 bytes). Otherwise an attacker-controlled count (up to 65535)
	// would eagerly allocate ~384KB per template record — and a datagram packed
	// with such records amplifies that into gigabytes of transient heap.
	avail := (len(body) - *p) / 4
	if avail < 0 {
		avail = 0
	}
	fields = make([]field, 0, min(count, avail))
	for i := 0; i < count; i++ {
		if *p+4 > len(body) {
			return nil, 0, false, false
		}
		ie := binary.BigEndian.Uint16(body[*p : *p+2])
		flen := binary.BigEndian.Uint16(body[*p+2 : *p+4])
		*p += 4
		ent := ie&enterpriseBit != 0
		if ent {
			if *p+4 > len(body) {
				return nil, 0, false, false
			}
			*p += 4 // skip the enterprise number
		}
		if flen == varLen {
			hasVar = true
		} else {
			recLen += int(flen)
		}
		fields = append(fields, field{ie: ie & 0x7fff, length: flen, enterprise: ent})
	}
	return fields, recLen, hasVar, true
}

func (d *Decoder) parseTemplates(exp string, obsDomain uint32, body []byte) {
	p := 0
	for p+4 <= len(body) {
		tid := binary.BigEndian.Uint16(body[p : p+2])
		fcount := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		p += 4
		if tid == 0 {
			break // padding
		}
		if fcount == 0 {
			d.deleteTemplate(templateKey{exp, obsDomain, tid}) // RFC 7011 withdrawal
			continue
		}
		fields, recLen, hasVar, ok := parseFieldSpecs(body, &p, fcount)
		if !ok {
			break
		}
		d.putTemplate(templateKey{exp, obsDomain, tid}, &template{fields: fields, recordLen: recLen, hasVarlen: hasVar})
		if d.templatesReceived != nil {
			d.templatesReceived.Inc()
		}
	}
}

func (d *Decoder) parseOptionsTemplates(exp string, obsDomain uint32, body []byte) {
	p := 0
	for p+6 <= len(body) {
		tid := binary.BigEndian.Uint16(body[p : p+2])
		fcount := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		// body[p+4:p+6] is scope_field_count; we don't need it to skip the data.
		p += 6
		if tid == 0 {
			break
		}
		if _, _, _, ok := parseFieldSpecs(body, &p, fcount); !ok {
			break
		}
		d.putTemplate(templateKey{exp, obsDomain, tid}, &template{isOptions: true})
		if d.templatesReceived != nil {
			d.templatesReceived.Inc()
		}
	}
}

func (d *Decoder) decodeData(dst []decoder.Flow, exp string, obsDomain uint32, setID uint16, body []byte, exportTime time.Time, exporter net.IP, limit int) []decoder.Flow {
	tmpl := d.getTemplate(templateKey{exp, obsDomain, setID})
	if tmpl == nil {
		if d.unknownTemplate != nil {
			d.unknownTemplate.Inc()
		}
		return dst
	}
	if tmpl.isOptions {
		return dst // control records, not flows
	}
	expIP := cloneIP(exporter)
	emitted := 0

	if !tmpl.hasVarlen {
		if tmpl.recordLen <= 0 {
			return dst
		}
		for pos := 0; pos+tmpl.recordLen <= len(body) && emitted < limit; pos += tmpl.recordLen {
			f, _, _ := decodeRecord(body[pos:pos+tmpl.recordLen], tmpl.fields, exportTime, expIP)
			dst = append(dst, f)
			emitted++
		}
		return dst
	}

	// Variable-length records: walk field-by-field to find each boundary.
	for pos := 0; pos < len(body) && emitted < limit; {
		f, n, ok := decodeRecord(body[pos:], tmpl.fields, exportTime, expIP)
		if !ok || n == 0 {
			break
		}
		dst = append(dst, f)
		emitted++
		pos += n
	}
	return dst
}

// decodeRecord decodes one record from the start of body, returning the flow,
// the number of bytes consumed, and whether it succeeded (false on truncation).
func decodeRecord(body []byte, fields []field, exportTime time.Time, expIP net.IP) (decoder.Flow, int, bool) {
	f := decoder.Flow{ExporterIP: expIP}
	pos := 0
	for _, fld := range fields {
		flen := int(fld.length)
		if fld.length == varLen {
			if pos >= len(body) {
				return f, 0, false
			}
			flen = int(body[pos])
			pos++
			if flen == 255 { // 3-byte length form
				if pos+2 > len(body) {
					return f, 0, false
				}
				flen = int(binary.BigEndian.Uint16(body[pos : pos+2]))
				pos += 2
			}
		}
		if pos+flen > len(body) {
			return f, 0, false
		}
		if !fld.enterprise {
			applyField(&f, fld.ie, body[pos:pos+flen], exportTime)
		}
		pos += flen
	}
	if f.FlowStart.IsZero() {
		f.FlowStart = exportTime
	}
	if f.FlowEnd.IsZero() {
		f.FlowEnd = exportTime
	}
	return f, pos, true
}

func applyField(f *decoder.Flow, ie uint16, v []byte, _ time.Time) {
	switch ie {
	case eOCTETS:
		f.Bytes = beUint(v)
	case ePOST_OCTETS:
		if f.Bytes == 0 {
			f.Bytes = beUint(v)
		}
	case ePKTS:
		f.Packets = beUint(v)
	case ePROTOCOL:
		f.Protocol = uint8(beUint(v))
	case eTOS:
		f.TOS = uint8(beUint(v))
	case eTCP_FLAGS:
		f.TCPFlags = uint8(beUint(v))
	case eSRC_PORT:
		f.SrcPort = uint16(beUint(v))
	case eDST_PORT:
		f.DstPort = uint16(beUint(v))
	case eSRC_IPV4, eSRC_IPV6:
		f.SrcIP = cloneIP(v)
	case eDST_IPV4, eDST_IPV6:
		f.DstIP = cloneIP(v)
	case eFLOW_START_S:
		f.FlowStart = time.Unix(int64(beUint(v)), 0).UTC()
	case eFLOW_END_S:
		f.FlowEnd = time.Unix(int64(beUint(v)), 0).UTC()
	case eFLOW_START_MS:
		f.FlowStart = msToTime(int64(beUint(v)))
	case eFLOW_END_MS:
		f.FlowEnd = msToTime(int64(beUint(v)))
	case eNAT_SRC_IPV4:
		f.NatPublicIP = cloneIP(v)
	case eNAT_SRC_PORT:
		f.NatPublicPort = uint16(beUint(v))
	}
}

func (d *Decoder) putTemplate(key templateKey, t *template) {
	now := time.Now()
	t.lastSeen.Store(now.UnixNano())
	d.mu.Lock()
	defer d.mu.Unlock()
	d.templates[key] = t
	if now.UnixNano()-d.lastSweep > int64(sweepEvery) || len(d.templates) > maxTemplates {
		d.sweepLocked(now.UnixNano())
	}
}

func (d *Decoder) deleteTemplate(key templateKey) {
	d.mu.Lock()
	delete(d.templates, key)
	d.mu.Unlock()
}

func (d *Decoder) getTemplate(key templateKey) *template {
	d.mu.RLock()
	t := d.templates[key]
	d.mu.RUnlock()
	if t != nil {
		t.lastSeen.Store(time.Now().UnixNano())
	}
	return t
}

func (d *Decoder) sweepLocked(nowNanos int64) {
	d.lastSweep = nowNanos
	ttl := int64(templateTTL)
	for k, t := range d.templates {
		if nowNanos-t.lastSeen.Load() > ttl {
			delete(d.templates, k)
		}
	}
	for len(d.templates) > maxTemplates {
		var oldestKey templateKey
		var oldest int64 = 1<<63 - 1
		for k, t := range d.templates {
			if ls := t.lastSeen.Load(); ls < oldest {
				oldest, oldestKey = ls, k
			}
		}
		delete(d.templates, oldestKey)
	}
}

func beUint(b []byte) uint64 {
	if len(b) > 8 {
		b = b[len(b)-8:]
	}
	var x uint64
	for _, c := range b {
		x = x<<8 | uint64(c)
	}
	return x
}

func cloneIP(b []byte) net.IP {
	if len(b) == 0 {
		return nil
	}
	out := make(net.IP, len(b))
	copy(out, b)
	return out
}

func msToTime(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}
