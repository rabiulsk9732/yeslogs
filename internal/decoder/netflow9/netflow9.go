// Package netflow9 implements a NetFlow version 9 decoder (RFC 3954).
//
// NetFlow v9 is template-based: exporters periodically send Template FlowSets
// that define the layout of subsequent Data FlowSets. This decoder caches
// templates per (exporter, source-id, template-id), decodes data records
// against them, and handles Options Templates (whose data records are skipped,
// not emitted as flows). It is safe for concurrent use.
package netflow9

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
	version   = 9
	headerLen = 20

	flowsetTemplate        = 0 // Template FlowSet
	flowsetOptionsTemplate = 1 // Options Template FlowSet
	flowsetDataMin         = 256

	// templateTTL bounds how long an idle (no longer refreshed or used)
	// template is kept. Active templates are refreshed on every use, so this
	// only evicts templates from exporters that have gone away.
	templateTTL  = 30 * time.Minute
	sweepEvery   = time.Minute
	maxTemplates = 1 << 16 // hard cap to bound memory against spoofed exporters

	// maxRecordsPerPacket bounds how many flow records a single datagram may
	// produce. A legitimate ~64KB v9 packet holds at most a few thousand
	// records; this cap blocks a flow-amplification DoS where a hostile packet
	// pairs a tiny record length with a maximally-sized data flowset.
	maxRecordsPerPacket = 8192
)

var (
	// ErrShortPacket is returned when the datagram cannot hold a v9 header.
	ErrShortPacket = errors.New("netflow9: packet shorter than header")
	// ErrWrongVersion is returned when the version field is not 9.
	ErrWrongVersion = errors.New("netflow9: not a version 9 packet")
)

// NetFlow v9 / IPFIX field type IDs we map onto decoder.Flow.
const (
	fIN_BYTES       = 1
	fIN_PKTS        = 2
	fPROTOCOL       = 4
	fTOS            = 5
	fTCP_FLAGS      = 6
	fL4_SRC_PORT    = 7
	fIPV4_SRC_ADDR  = 8
	fL4_DST_PORT    = 11
	fIPV4_DST_ADDR  = 12
	fLAST_SWITCHED  = 21
	fFIRST_SWITCHED = 22
	fOUT_BYTES      = 23
	fIPV6_SRC_ADDR  = 27
	fIPV6_DST_ADDR  = 28
	fNAT_SRC_IPV4   = 225 // postNATSourceIPv4Address
	fNAT_SRC_PORT   = 227 // postNAPTSourceTransportPort
)

type field struct {
	typ    uint16
	length uint16
}

type template struct {
	fields    []field
	recordLen int
	isOptions bool
	lastSeen  atomic.Int64 // unix nanos, refreshed on use
}

type templateKey struct {
	exporter string
	sourceID uint32
	id       uint16
}

// Decoder decodes NetFlow v9 datagrams, caching templates across packets.
type Decoder struct {
	mu        sync.RWMutex
	templates map[templateKey]*template
	lastSweep int64 // unix nanos

	templatesReceived prometheus.Counter
	unknownTemplate   prometheus.Counter
}

// New returns a NetFlow v9 decoder. The two counters are optional (may be nil);
// they record templates parsed and data flowsets referencing an unknown
// template, respectively.
func New(templatesReceived, unknownTemplate prometheus.Counter) *Decoder {
	return &Decoder{
		templates:         make(map[templateKey]*template),
		templatesReceived: templatesReceived,
		unknownTemplate:   unknownTemplate,
	}
}

// Kind implements decoder.Decoder.
func (d *Decoder) Kind() string { return "netflow9" }

// Decode implements decoder.Decoder.
func (d *Decoder) Decode(dst []decoder.Flow, payload []byte, exporter net.IP) ([]decoder.Flow, error) {
	if len(payload) < headerLen {
		return dst, ErrShortPacket
	}
	if binary.BigEndian.Uint16(payload[0:2]) != version {
		return dst, ErrWrongVersion
	}

	sysUptime := uint64(binary.BigEndian.Uint32(payload[4:8]))
	unixSecs := uint64(binary.BigEndian.Uint32(payload[8:12]))
	sourceID := binary.BigEndian.Uint32(payload[16:20])
	bootMS := int64(unixSecs)*1000 - int64(sysUptime)
	exp := exporter.String()

	budget := maxRecordsPerPacket
	off := headerLen
	for off+4 <= len(payload) {
		fsID := binary.BigEndian.Uint16(payload[off : off+2])
		fsLen := int(binary.BigEndian.Uint16(payload[off+2 : off+4]))
		if fsLen < 4 || off+fsLen > len(payload) {
			break // malformed or truncated: stop parsing this datagram
		}
		body := payload[off+4 : off+fsLen]

		switch {
		case fsID == flowsetTemplate:
			d.parseTemplates(exp, sourceID, body, false)
		case fsID == flowsetOptionsTemplate:
			d.parseOptionsTemplate(exp, sourceID, body)
		case fsID >= flowsetDataMin:
			before := len(dst)
			dst = d.decodeData(dst, exp, sourceID, uint16(fsID), body, bootMS, exporter, budget)
			if budget -= len(dst) - before; budget <= 0 {
				return dst, nil // per-packet record cap reached
			}
		default:
			// flowset IDs 2..255 are reserved; ignore.
		}
		off += fsLen
	}
	return dst, nil
}

// parseTemplates parses a Template FlowSet body (one or more templates).
func (d *Decoder) parseTemplates(exp string, sourceID uint32, body []byte, _ bool) {
	p := 0
	for p+4 <= len(body) {
		tid := binary.BigEndian.Uint16(body[p : p+2])
		fcount := int(binary.BigEndian.Uint16(body[p+2 : p+4]))
		p += 4
		if fcount == 0 || p+fcount*4 > len(body) {
			break
		}
		fields := make([]field, fcount)
		recLen := 0
		for i := 0; i < fcount; i++ {
			ft := binary.BigEndian.Uint16(body[p : p+2])
			fl := binary.BigEndian.Uint16(body[p+2 : p+4])
			p += 4
			fields[i] = field{typ: ft, length: fl}
			recLen += int(fl)
		}
		d.putTemplate(templateKey{exp, sourceID, tid}, &template{fields: fields, recordLen: recLen})
		if d.templatesReceived != nil {
			d.templatesReceived.Inc()
		}
	}
}

// parseOptionsTemplate parses an Options Template FlowSet. We only need the
// record length so the matching data flowset can be skipped cleanly (its
// records are control data, not flows).
func (d *Decoder) parseOptionsTemplate(exp string, sourceID uint32, body []byte) {
	if len(body) < 6 {
		return
	}
	tid := binary.BigEndian.Uint16(body[0:2])
	scopeLen := int(binary.BigEndian.Uint16(body[2:4]))
	optLen := int(binary.BigEndian.Uint16(body[4:6]))
	p := 6
	total := scopeLen + optLen
	if total <= 0 || p+total > len(body) {
		return
	}
	recLen := 0
	for q := p; q+4 <= p+total; q += 4 {
		recLen += int(binary.BigEndian.Uint16(body[q+2 : q+4]))
	}
	d.putTemplate(templateKey{exp, sourceID, tid}, &template{recordLen: recLen, isOptions: true})
	if d.templatesReceived != nil {
		d.templatesReceived.Inc()
	}
}

// decodeData decodes a Data FlowSet against its cached template, emitting at
// most limit records (the remaining per-datagram budget).
func (d *Decoder) decodeData(dst []decoder.Flow, exp string, sourceID uint32, tid uint16, body []byte, bootMS int64, exporter net.IP, limit int) []decoder.Flow {
	tmpl := d.getTemplate(templateKey{exp, sourceID, tid})
	if tmpl == nil {
		if d.unknownTemplate != nil {
			d.unknownTemplate.Inc()
		}
		return dst
	}
	if tmpl.isOptions || tmpl.recordLen <= 0 {
		return dst // control records or degenerate template: nothing to emit
	}

	expIP := cloneIP(exporter)
	emitted := 0
	for pos := 0; pos+tmpl.recordLen <= len(body) && emitted < limit; pos += tmpl.recordLen {
		emitted++
		rec := body[pos : pos+tmpl.recordLen]
		f := decoder.Flow{ExporterIP: expIP}
		fp := 0
		for _, fld := range tmpl.fields {
			applyField(&f, fld.typ, rec[fp:fp+int(fld.length)], bootMS)
			fp += int(fld.length)
		}
		dst = append(dst, f)
	}
	return dst
}

// applyField maps one decoded field value onto the Flow.
func applyField(f *decoder.Flow, typ uint16, v []byte, bootMS int64) {
	switch typ {
	case fIN_BYTES:
		f.Bytes = beUint(v)
	case fOUT_BYTES:
		if f.Bytes == 0 {
			f.Bytes = beUint(v)
		}
	case fIN_PKTS:
		f.Packets = beUint(v)
	case fPROTOCOL:
		f.Protocol = uint8(beUint(v))
	case fTOS:
		f.TOS = uint8(beUint(v))
	case fTCP_FLAGS:
		f.TCPFlags = uint8(beUint(v))
	case fL4_SRC_PORT:
		f.SrcPort = uint16(beUint(v))
	case fL4_DST_PORT:
		f.DstPort = uint16(beUint(v))
	case fIPV4_SRC_ADDR, fIPV6_SRC_ADDR:
		f.SrcIP = cloneIP(v)
	case fIPV4_DST_ADDR, fIPV6_DST_ADDR:
		f.DstIP = cloneIP(v)
	case fFIRST_SWITCHED:
		f.FlowStart = msToTime(bootMS + int64(beUint(v)))
	case fLAST_SWITCHED:
		f.FlowEnd = msToTime(bootMS + int64(beUint(v)))
	case fNAT_SRC_IPV4:
		f.NatPublicIP = cloneIP(v)
	case fNAT_SRC_PORT:
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

func (d *Decoder) getTemplate(key templateKey) *template {
	d.mu.RLock()
	t := d.templates[key]
	d.mu.RUnlock()
	if t != nil {
		t.lastSeen.Store(time.Now().UnixNano()) // refresh-on-use, lock-free
	}
	return t
}

// sweepLocked evicts expired templates and, if still over the cap, the oldest.
// Caller must hold the write lock.
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

// beUint reads a big-endian unsigned integer from up to 8 bytes.
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
