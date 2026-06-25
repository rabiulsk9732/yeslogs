package ipfix

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// --- builders ---------------------------------------------------------------

func be16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

type spec struct {
	ie     uint16
	length uint16
	ent    bool
	pen    uint32
}

func ipfixMsg(exportSecs, obsDomain uint32, sets ...[]byte) []byte {
	var body []byte
	for _, s := range sets {
		body = append(body, s...)
	}
	h := make([]byte, headerLen)
	be16(h[0:2], version)
	be16(h[2:4], uint16(headerLen+len(body)))
	be32(h[4:8], exportSecs)
	be32(h[8:12], 1)
	be32(h[12:16], obsDomain)
	return append(h, body...)
}

func set(setID uint16, body []byte) []byte {
	s := make([]byte, 4+len(body))
	be16(s[0:2], setID)
	be16(s[2:4], uint16(len(s)))
	copy(s[4:], body)
	return s
}

func templateRecord(tid uint16, specs []spec) []byte {
	b := make([]byte, 4)
	be16(b[0:2], tid)
	be16(b[2:4], uint16(len(specs)))
	for _, s := range specs {
		ie := s.ie
		if s.ent {
			ie |= enterpriseBit
		}
		f := make([]byte, 4)
		be16(f[0:2], ie)
		be16(f[2:4], s.length)
		b = append(b, f...)
		if s.ent {
			p := make([]byte, 4)
			be32(p, s.pen)
			b = append(b, p...)
		}
	}
	return b
}

func u16b(v uint16) []byte { b := make([]byte, 2); be16(b, v); return b }
func u32b(v uint32) []byte { b := make([]byte, 4); be32(b, v); return b }
func u64b(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// --- tests ------------------------------------------------------------------

const exSecs = uint32(1_700_000_000)

func TestTemplateAndDataWithNATAndMs(t *testing.T) {
	d := New(nil, nil)
	specs := []spec{
		{ie: eSRC_IPV4, length: 4}, {ie: eDST_IPV4, length: 4},
		{ie: eSRC_PORT, length: 2}, {ie: eDST_PORT, length: 2}, {ie: ePROTOCOL, length: 1},
		{ie: eOCTETS, length: 4}, {ie: ePKTS, length: 4},
		{ie: eFLOW_START_MS, length: 8}, {ie: eFLOW_END_MS, length: 8},
		{ie: eNAT_SRC_IPV4, length: 4}, {ie: eNAT_SRC_PORT, length: 2},
	}
	startMs := uint64(exSecs)*1000 - 5000

	var rec []byte
	rec = append(rec, net.IPv4(10, 0, 0, 7).To4()...)
	rec = append(rec, net.IPv4(8, 8, 4, 4).To4()...)
	rec = append(rec, u16b(33000)...)
	rec = append(rec, u16b(443)...)
	rec = append(rec, byte(6))
	rec = append(rec, u32b(9000)...)
	rec = append(rec, u32b(12)...)
	rec = append(rec, u64b(startMs)...)
	rec = append(rec, u64b(startMs+1000)...)
	rec = append(rec, net.IPv4(203, 0, 113, 9).To4()...)
	rec = append(rec, u16b(40001)...)

	msg := ipfixMsg(exSecs, 99, set(setTemplate, templateRecord(256, specs)), set(256, rec))

	flows, err := d.Decode(nil, msg, net.IPv4(192, 0, 2, 1))
	if err != nil || len(flows) != 1 {
		t.Fatalf("decode: flows=%d err=%v", len(flows), err)
	}
	f := flows[0]
	if !f.SrcIP.Equal(net.IPv4(10, 0, 0, 7)) || !f.DstIP.Equal(net.IPv4(8, 8, 4, 4)) {
		t.Errorf("ips = %v -> %v", f.SrcIP, f.DstIP)
	}
	if f.SrcPort != 33000 || f.DstPort != 443 || f.Protocol != 6 {
		t.Errorf("ports/proto = %d/%d/%d", f.SrcPort, f.DstPort, f.Protocol)
	}
	if f.Bytes != 9000 || f.Packets != 12 {
		t.Errorf("bytes/pkts = %d/%d", f.Bytes, f.Packets)
	}
	if f.FlowStart.UnixMilli() != int64(startMs) {
		t.Errorf("flowStart = %d want %d", f.FlowStart.UnixMilli(), startMs)
	}
	if !f.NatPublicIP.Equal(net.IPv4(203, 0, 113, 9)) || f.NatPublicPort != 40001 {
		t.Errorf("nat = %v:%d", f.NatPublicIP, f.NatPublicPort)
	}
}

func TestEnterpriseFieldSkipped(t *testing.T) {
	d := New(nil, nil)
	specs := []spec{
		{ie: eSRC_IPV4, length: 4},
		{ie: 1, length: 4, ent: true, pen: 12345}, // enterprise field, 4 bytes, must be skipped
		{ie: eDST_IPV4, length: 4},
		{ie: eOCTETS, length: 4},
	}
	var rec []byte
	rec = append(rec, net.IPv4(10, 1, 1, 1).To4()...)
	rec = append(rec, u32b(0xAABBCCDD)...) // opaque enterprise value
	rec = append(rec, net.IPv4(1, 1, 1, 1).To4()...)
	rec = append(rec, u32b(777)...)

	msg := ipfixMsg(exSecs, 1, set(setTemplate, templateRecord(300, specs)), set(300, rec))
	flows, err := d.Decode(nil, msg, net.IPv4(192, 0, 2, 9))
	if err != nil || len(flows) != 1 {
		t.Fatalf("decode: flows=%d err=%v", len(flows), err)
	}
	f := flows[0]
	if !f.SrcIP.Equal(net.IPv4(10, 1, 1, 1)) || !f.DstIP.Equal(net.IPv4(1, 1, 1, 1)) {
		t.Errorf("enterprise field misaligned record: %v -> %v", f.SrcIP, f.DstIP)
	}
	if f.Bytes != 777 {
		t.Errorf("bytes = %d want 777", f.Bytes)
	}
}

func TestVariableLengthFields(t *testing.T) {
	d := New(nil, nil)
	specs := []spec{
		{ie: eSRC_IPV4, length: 4}, {ie: eDST_IPV4, length: 4},
		{ie: 82, length: varLen}, // interfaceName, variable length, skipped
		{ie: eOCTETS, length: 4},
	}
	// record builder: src(4) dst(4) [len][name] octets(4), short form length.
	mkrec := func(name string, octets uint32) []byte {
		var r []byte
		r = append(r, net.IPv4(10, 0, 0, 1).To4()...)
		r = append(r, net.IPv4(2, 2, 2, 2).To4()...)
		r = append(r, byte(len(name)))
		r = append(r, []byte(name)...)
		r = append(r, u32b(octets)...)
		return r
	}
	body := append(mkrec("eth0", 100), mkrec("eth0/0/1", 200)...)
	msg := ipfixMsg(exSecs, 1, set(setTemplate, templateRecord(320, specs)), set(320, body))

	flows, err := d.Decode(nil, msg, net.IPv4(192, 0, 2, 1))
	if err != nil || len(flows) != 2 {
		t.Fatalf("varlen decode: flows=%d err=%v", len(flows), err)
	}
	if flows[0].Bytes != 100 || flows[1].Bytes != 200 {
		t.Errorf("varlen record boundaries wrong: %d, %d", flows[0].Bytes, flows[1].Bytes)
	}
}

func TestOptionsTemplateSkippedAndUnknownCounted(t *testing.T) {
	unknown := prometheus.NewCounter(prometheus.CounterOpts{Name: "u"})
	d := New(nil, unknown)

	// Options template id 400 (scope_field_count then specs). Its data set must
	// be skipped (no flows) and NOT counted as unknown.
	ot := make([]byte, 6)
	be16(ot[0:2], 400)                  // template id
	be16(ot[2:4], 1)                    // field count
	be16(ot[4:6], 1)                    // scope field count
	ot = append(ot, u16b(eSRC_IPV4)...) // one (scope) field spec: ie
	ot = append(ot, u16b(4)...)         // length
	optData := set(400, net.IPv4(10, 0, 0, 1).To4())
	msg := ipfixMsg(exSecs, 1, set(setOptionsTemplate, ot), optData)

	flows, err := d.Decode(nil, msg, net.IPv4(192, 0, 2, 1))
	if err != nil || len(flows) != 0 {
		t.Fatalf("options data should yield 0 flows: got %d err=%v", len(flows), err)
	}
	if v := testutil.ToFloat64(unknown); v != 0 {
		t.Errorf("options data wrongly counted as unknown: %v", v)
	}

	// A data set for a never-seen template id IS counted unknown.
	if _, err := d.Decode(nil, ipfixMsg(exSecs, 1, set(999, []byte{1, 2, 3, 4})), net.IPv4(192, 0, 2, 1)); err != nil {
		t.Fatal(err)
	}
	if v := testutil.ToFloat64(unknown); v != 1 {
		t.Errorf("unknown template count = %v want 1", v)
	}
}

func TestAmplificationCap(t *testing.T) {
	d := New(nil, nil)
	exporter := net.IPv4(10, 0, 0, 254)
	// Template with a single 1-byte field -> recordLen 1.
	tmpl := ipfixMsg(exSecs, 1, set(setTemplate, templateRecord(256, []spec{{ie: ePROTOCOL, length: 1}})))
	if _, err := d.Decode(nil, tmpl, exporter); err != nil {
		t.Fatal(err)
	}
	const n = 60000
	data := ipfixMsg(exSecs, 1, set(256, make([]byte, n)))
	flows, err := d.Decode(nil, data, exporter)
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) > maxRecordsPerPacket {
		t.Fatalf("amplification not capped: %d > %d", len(flows), maxRecordsPerPacket)
	}
}

// TestHugeFieldCountSafe ensures a template record claiming a huge field count
// with no field bytes present does not over-allocate, panic, or register a
// template (regression for the field-count amplification DoS).
func TestHugeFieldCountSafe(t *testing.T) {
	d := New(nil, nil)
	// A template set whose only record is tid(2)+fcount(0xFFFF) with no specs.
	rec := make([]byte, 4)
	be16(rec[0:2], 256)
	be16(rec[2:4], 0xFFFF)
	// Pack many such tiny template sets into one datagram.
	var sets [][]byte
	for i := 0; i < 2000; i++ {
		sets = append(sets, set(setTemplate, rec))
	}
	msg := ipfixMsg(exSecs, 1, sets...)

	flows, err := d.Decode(nil, msg, net.IPv4(192, 0, 2, 1))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flows) != 0 {
		t.Fatalf("want 0 flows, got %d", len(flows))
	}
	// No valid template should have been registered.
	data := ipfixMsg(exSecs, 1, set(256, make([]byte, 40)))
	if f, _ := d.Decode(nil, data, net.IPv4(192, 0, 2, 1)); len(f) != 0 {
		t.Fatalf("bogus template must not decode data, got %d flows", len(f))
	}
}

// TestTemplateWithdrawal verifies a field_count==0 record evicts the template.
func TestTemplateWithdrawal(t *testing.T) {
	d := New(nil, nil)
	exporter := net.IPv4(192, 0, 2, 5)
	specs := []spec{{ie: eSRC_IPV4, length: 4}, {ie: eDST_IPV4, length: 4}, {ie: eOCTETS, length: 4}}

	var rec []byte
	rec = append(rec, net.IPv4(10, 0, 0, 1).To4()...)
	rec = append(rec, net.IPv4(1, 1, 1, 1).To4()...)
	rec = append(rec, u32b(500)...)
	if f, _ := d.Decode(nil, ipfixMsg(exSecs, 1, set(setTemplate, templateRecord(256, specs)), set(256, rec)), exporter); len(f) != 1 {
		t.Fatalf("want 1 flow before withdrawal, got %d", len(f))
	}

	// Withdrawal: template record with field_count 0.
	wd := make([]byte, 4)
	be16(wd[0:2], 256)
	be16(wd[2:4], 0)
	if _, err := d.Decode(nil, ipfixMsg(exSecs, 1, set(setTemplate, wd)), exporter); err != nil {
		t.Fatal(err)
	}

	if f, _ := d.Decode(nil, ipfixMsg(exSecs, 1, set(256, rec)), exporter); len(f) != 0 {
		t.Fatalf("template should be withdrawn; got %d flows", len(f))
	}
}

func TestMalformedNoPanic(t *testing.T) {
	d := New(nil, nil)
	if _, err := d.Decode(nil, []byte{0, 10, 0, 4}, nil); err == nil {
		t.Error("want error for short packet")
	}
	bad := make([]byte, headerLen)
	be16(bad[0:2], 9)
	if _, err := d.Decode(nil, bad, nil); err == nil {
		t.Error("want error for wrong version")
	}
	// Truncated set must not panic.
	pkt := ipfixMsg(exSecs, 1, []byte{0, 2, 0xFF, 0xFF}) // set claims 65535 bytes
	if _, err := d.Decode(nil, pkt, net.IPv4(1, 1, 1, 1)); err != nil {
		t.Errorf("truncated set should be tolerated: %v", err)
	}
}
