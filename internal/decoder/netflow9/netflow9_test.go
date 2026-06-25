package netflow9

import (
	"encoding/binary"
	"net"
	"testing"
)

// --- packet builders --------------------------------------------------------

func v9Header(count uint16, sysUptime, unixSecs, seq, sourceID uint32) []byte {
	h := make([]byte, headerLen)
	binary.BigEndian.PutUint16(h[0:2], version)
	binary.BigEndian.PutUint16(h[2:4], count)
	binary.BigEndian.PutUint32(h[4:8], sysUptime)
	binary.BigEndian.PutUint32(h[8:12], unixSecs)
	binary.BigEndian.PutUint32(h[12:16], seq)
	binary.BigEndian.PutUint32(h[16:20], sourceID)
	return h
}

// stdTemplateFields is a typical IPv4 flow template (record length 29).
var stdTemplateFields = []field{
	{fIPV4_SRC_ADDR, 4}, {fIPV4_DST_ADDR, 4},
	{fL4_SRC_PORT, 2}, {fL4_DST_PORT, 2},
	{fPROTOCOL, 1}, {fIN_BYTES, 4}, {fIN_PKTS, 4},
	{fFIRST_SWITCHED, 4}, {fLAST_SWITCHED, 4},
}

func templateFlowSet(tid uint16, fields []field) []byte {
	body := make([]byte, 4+len(fields)*4)
	binary.BigEndian.PutUint16(body[0:2], tid)
	binary.BigEndian.PutUint16(body[2:4], uint16(len(fields)))
	for i, f := range fields {
		o := 4 + i*4
		binary.BigEndian.PutUint16(body[o:o+2], f.typ)
		binary.BigEndian.PutUint16(body[o+2:o+4], f.length)
	}
	fs := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(fs[0:2], flowsetTemplate)
	binary.BigEndian.PutUint16(fs[2:4], uint16(len(fs)))
	copy(fs[4:], body)
	return fs
}

func stdRecord(src, dst net.IP, sp, dp uint16, proto uint8, bytes, pkts, first, last uint32) []byte {
	r := make([]byte, 29)
	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint16(r[8:10], sp)
	binary.BigEndian.PutUint16(r[10:12], dp)
	r[12] = proto
	binary.BigEndian.PutUint32(r[13:17], bytes)
	binary.BigEndian.PutUint32(r[17:21], pkts)
	binary.BigEndian.PutUint32(r[21:25], first)
	binary.BigEndian.PutUint32(r[25:29], last)
	return r
}

func dataFlowSet(tid uint16, records ...[]byte) []byte {
	body := []byte{}
	for _, r := range records {
		body = append(body, r...)
	}
	fs := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(fs[0:2], tid)
	binary.BigEndian.PutUint16(fs[2:4], uint16(len(fs)))
	copy(fs[4:], body)
	return fs
}

// --- tests ------------------------------------------------------------------

const (
	tUptime = uint32(3_600_000)
	tSecs   = uint32(1_700_000_000)
)

func TestTemplateThenData(t *testing.T) {
	d := New(nil, nil)
	exporter := net.IPv4(192, 168, 1, 1)

	// 1. Data before any template -> nothing, unknown template.
	data := dataFlowSet(256, stdRecord(net.IPv4(10, 0, 0, 1), net.IPv4(1, 1, 1, 1), 1234, 443, 6, 500, 4, tUptime-2000, tUptime-1000))
	pkt := append(v9Header(1, tUptime, tSecs, 1, 7), data...)
	flows, err := d.Decode(nil, pkt, exporter)
	if err != nil {
		t.Fatalf("decode data-first: %v", err)
	}
	if len(flows) != 0 {
		t.Fatalf("want 0 flows before template, got %d", len(flows))
	}

	// 2. Template packet -> cached, no flows.
	tmpl := append(v9Header(1, tUptime, tSecs, 2, 7), templateFlowSet(256, stdTemplateFields)...)
	if flows, err = d.Decode(nil, tmpl, exporter); err != nil || len(flows) != 0 {
		t.Fatalf("template packet: flows=%d err=%v", len(flows), err)
	}

	// 3. Data again -> now decoded.
	flows, err = d.Decode(nil, pkt, exporter)
	if err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("want 1 flow, got %d", len(flows))
	}
	f := flows[0]
	if !f.SrcIP.Equal(net.IPv4(10, 0, 0, 1)) || !f.DstIP.Equal(net.IPv4(1, 1, 1, 1)) {
		t.Errorf("ips = %v -> %v", f.SrcIP, f.DstIP)
	}
	if f.SrcPort != 1234 || f.DstPort != 443 || f.Protocol != 6 {
		t.Errorf("ports/proto = %d/%d/%d", f.SrcPort, f.DstPort, f.Protocol)
	}
	if f.Bytes != 500 || f.Packets != 4 {
		t.Errorf("bytes/pkts = %d/%d", f.Bytes, f.Packets)
	}
	if !f.ExporterIP.Equal(exporter) {
		t.Errorf("exporter = %v", f.ExporterIP)
	}
	if got := f.FlowStart.UnixMilli(); got != int64(tSecs)*1000-2000 {
		t.Errorf("flowStart ms = %d, want %d", got, int64(tSecs)*1000-2000)
	}
}

func TestCombinedTemplateAndDataSamePacket(t *testing.T) {
	d := New(nil, nil)
	body := append(templateFlowSet(260, stdTemplateFields),
		dataFlowSet(260,
			stdRecord(net.IPv4(10, 0, 0, 2), net.IPv4(8, 8, 8, 8), 5, 53, 17, 90, 1, tUptime-500, tUptime-400),
			stdRecord(net.IPv4(10, 0, 0, 3), net.IPv4(9, 9, 9, 9), 6, 80, 6, 1200, 9, tUptime-300, tUptime-100),
		)...)
	pkt := append(v9Header(3, tUptime, tSecs, 1, 1), body...)

	flows, err := d.Decode(nil, pkt, net.IPv4(10, 0, 0, 254))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flows) != 2 {
		t.Fatalf("want 2 flows, got %d", len(flows))
	}
	if flows[0].DstPort != 53 || flows[1].DstPort != 80 {
		t.Errorf("dst ports = %d, %d", flows[0].DstPort, flows[1].DstPort)
	}
}

func TestNATFields(t *testing.T) {
	d := New(nil, nil)
	fields := []field{
		{fIPV4_SRC_ADDR, 4}, {fIPV4_DST_ADDR, 4},
		{fL4_SRC_PORT, 2}, {fL4_DST_PORT, 2}, {fPROTOCOL, 1},
		{fNAT_SRC_IPV4, 4}, {fNAT_SRC_PORT, 2},
	}
	rec := make([]byte, 4+4+2+2+1+4+2) // 19
	copy(rec[0:4], net.IPv4(10, 0, 0, 9).To4())
	copy(rec[4:8], net.IPv4(1, 1, 1, 1).To4())
	binary.BigEndian.PutUint16(rec[8:10], 4444)
	binary.BigEndian.PutUint16(rec[10:12], 443)
	rec[12] = 6
	copy(rec[13:17], net.IPv4(203, 0, 113, 5).To4())
	binary.BigEndian.PutUint16(rec[17:19], 50000)

	pkt := append(v9Header(2, tUptime, tSecs, 1, 1),
		append(templateFlowSet(300, fields), dataFlowSet(300, rec)...)...)

	flows, err := d.Decode(nil, pkt, net.IPv4(10, 0, 0, 254))
	if err != nil || len(flows) != 1 {
		t.Fatalf("decode nat: flows=%d err=%v", len(flows), err)
	}
	if !flows[0].NatPublicIP.Equal(net.IPv4(203, 0, 113, 5)) {
		t.Errorf("natPublicIP = %v", flows[0].NatPublicIP)
	}
	if flows[0].NatPublicPort != 50000 {
		t.Errorf("natPublicPort = %d", flows[0].NatPublicPort)
	}
}

func TestTemplateIsolationByExporter(t *testing.T) {
	d := New(nil, nil)
	tmpl := append(v9Header(1, tUptime, tSecs, 1, 5), templateFlowSet(256, stdTemplateFields)...)
	if _, err := d.Decode(nil, tmpl, net.IPv4(10, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	// Same template id, DIFFERENT exporter -> still unknown.
	data := append(v9Header(1, tUptime, tSecs, 1, 5),
		dataFlowSet(256, stdRecord(net.IPv4(10, 0, 0, 1), net.IPv4(1, 1, 1, 1), 1, 2, 6, 10, 1, tUptime-100, tUptime-50))...)
	flows, _ := d.Decode(nil, data, net.IPv4(10, 0, 0, 2)) // other exporter
	if len(flows) != 0 {
		t.Fatalf("template must be isolated per exporter; got %d flows", len(flows))
	}
}

// TestAmplificationCap ensures a hostile packet (1-byte record + huge data
// flowset) cannot make the decoder emit an unbounded number of flows.
func TestAmplificationCap(t *testing.T) {
	d := New(nil, nil)
	exporter := net.IPv4(10, 0, 0, 254)

	// Template with a single 1-byte field -> recordLen 1.
	tmpl := append(v9Header(1, tUptime, tSecs, 1, 1),
		templateFlowSet(256, []field{{fPROTOCOL, 1}})...)
	if _, err := d.Decode(nil, tmpl, exporter); err != nil {
		t.Fatalf("template: %v", err)
	}

	// One big data flowset: header(4) + 60000 one-byte records.
	const n = 60000
	fs := make([]byte, 4+n)
	binary.BigEndian.PutUint16(fs[0:2], 256)
	binary.BigEndian.PutUint16(fs[2:4], uint16(4+n)) // fits in uint16 only up to 65535
	pkt := append(v9Header(1, tUptime, tSecs, 2, 1), fs...)

	flows, err := d.Decode(nil, pkt, exporter)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flows) > maxRecordsPerPacket {
		t.Fatalf("amplification not capped: emitted %d > %d", len(flows), maxRecordsPerPacket)
	}
}

func TestMalformed(t *testing.T) {
	d := New(nil, nil)
	if _, err := d.Decode(nil, []byte{0, 9, 0, 1}, nil); err == nil {
		t.Error("want error for short packet")
	}
	bad := make([]byte, headerLen)
	binary.BigEndian.PutUint16(bad[0:2], 5) // wrong version
	if _, err := d.Decode(nil, bad, nil); err == nil {
		t.Error("want error for wrong version")
	}
	// Truncated flowset length must not panic.
	pkt := v9Header(1, tUptime, tSecs, 1, 1)
	fs := []byte{1, 0, 0, 200} // claims 200 bytes but none follow
	pkt = append(pkt, fs...)
	if _, err := d.Decode(nil, pkt, net.IPv4(1, 1, 1, 1)); err != nil {
		t.Errorf("truncated flowset should be tolerated, got %v", err)
	}
}
