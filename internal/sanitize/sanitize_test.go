package sanitize

import (
	"bytes"
	"encoding/binary"
	"net"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow9"
	"github.com/natflow/natflow-dataplane/internal/pcapio"
)

func be16(b []byte, v uint16) { binary.BigEndian.PutUint16(b, v) }
func be32(b []byte, v uint32) { binary.BigEndian.PutUint32(b, v) }

func v5Record(src, dst net.IP) []byte {
	r := make([]byte, 48)
	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	copy(r[8:12], net.IPv4(192, 168, 0, 1).To4()) // nexthop
	be32(r[16:20], 5)                             // packets
	be32(r[20:24], 600)                           // bytes
	be16(r[32:34], 1234)
	be16(r[34:36], 53)
	r[38] = 17
	return r
}

func v5Packet(recs ...[]byte) []byte {
	h := make([]byte, 24)
	be16(h[0:2], 5)
	be16(h[2:4], uint16(len(recs)))
	be32(h[8:12], 1_700_000_000)
	out := h
	for _, r := range recs {
		out = append(out, r...)
	}
	return out
}

func ethFrame(src, dst [4]byte, payload []byte) []byte {
	return pcapio.EthernetIPv4UDP(src, dst, 40000, 2055, payload)
}

func TestSanitizeV5(t *testing.T) {
	realSrc, realDst := net.IPv4(100, 64, 0, 1), net.IPv4(8, 8, 8, 8)
	payload := v5Packet(v5Record(realSrc, realDst))
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, payload)

	s := New([]byte("fixed-test-key"))
	if !s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("frame unexpectedly dropped")
	}

	// No original address may remain anywhere in the frame.
	for _, ip := range [][]byte{realSrc.To4(), realDst.To4(), {203, 0, 113, 5}, {198, 51, 100, 10}, {192, 168, 0, 1}} {
		if bytes.Contains(frame, ip) {
			t.Errorf("original address %v leaked into sanitized frame", net.IP(ip))
		}
	}

	// Payload must still decode (structure preserved), with remapped 10.x IPs.
	info, _ := pcapio.ParseUDP(pcapio.LinkEthernet, frame)
	payOut := frame[info.PayloadOff : info.PayloadOff+info.PayloadLen]
	flows, err := netflow5.New().Decode(nil, payOut, net.IPv4(10, 0, 0, 1))
	if err != nil || len(flows) != 1 {
		t.Fatalf("decode after sanitize: flows=%d err=%v", len(flows), err)
	}
	if flows[0].SrcIP.To4()[0] != 10 || flows[0].DstIP.To4()[0] != 10 {
		t.Errorf("payload IPs not remapped to 10.x: %v -> %v", flows[0].SrcIP, flows[0].DstIP)
	}
	if s.Stats.IPsRewritten == 0 {
		t.Error("no IPs rewritten")
	}
}

func TestSanitizeDeterministicWithKey(t *testing.T) {
	mk := func() []byte {
		f := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, v5Packet(v5Record(net.IPv4(100, 64, 0, 9), net.IPv4(1, 1, 1, 1))))
		New([]byte("k")).Frame(pcapio.LinkEthernet, f)
		return f
	}
	if !bytes.Equal(mk(), mk()) {
		t.Error("same key must produce identical sanitized output")
	}
}

// --- NetFlow v9 ---

func v9Template(tid uint16, fields [][2]uint16) []byte {
	body := make([]byte, 4)
	be16(body[0:2], tid)
	be16(body[2:4], uint16(len(fields)))
	for _, f := range fields {
		x := make([]byte, 4)
		be16(x[0:2], f[0])
		be16(x[2:4], f[1])
		body = append(body, x...)
	}
	fs := make([]byte, 4)
	be16(fs[0:2], 0)
	be16(fs[2:4], uint16(4+len(body)))
	return append(fs, body...)
}

func v9Data(tid uint16, records ...[]byte) []byte {
	body := []byte{}
	for _, r := range records {
		body = append(body, r...)
	}
	fs := make([]byte, 4)
	be16(fs[0:2], tid)
	be16(fs[2:4], uint16(4+len(body)))
	return append(fs, body...)
}

func v9Packet(domain uint32, sets ...[]byte) []byte {
	h := make([]byte, 20)
	be16(h[0:2], 9)
	be16(h[2:4], 1)
	be32(h[16:20], domain)
	out := h
	for _, s := range sets {
		out = append(out, s...)
	}
	return out
}

func TestSanitizeV9UnknownTemplateDropped(t *testing.T) {
	// Data flowset with no preceding template -> cannot sanitize -> dropped.
	rec := make([]byte, 13)
	copy(rec[0:4], net.IPv4(100, 64, 0, 7).To4())
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, v9Packet(1, v9Data(256, rec)))
	s := New([]byte("k"))
	if s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("v9 data with unknown template must be dropped")
	}
	if s.Stats.Dropped != 1 {
		t.Errorf("dropped = %d, want 1", s.Stats.Dropped)
	}
}

func TestSanitizeNonIPv4Dropped(t *testing.T) {
	// ARP-like frame (ethertype 0x0806) carrying a real-looking address.
	frame := make([]byte, 28)
	frame[12], frame[13] = 0x08, 0x06
	copy(frame[14:18], net.IPv4(100, 64, 0, 7).To4())
	s := New([]byte("k"))
	if s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("non-IPv4 frame must be dropped")
	}
}

func TestSanitizeNonUDPIPv4HeaderRewritten(t *testing.T) {
	// Start from a UDP frame, flip the IP protocol to TCP (6).
	frame := ethFrame([4]byte{100, 64, 0, 7}, [4]byte{9, 9, 9, 9}, v5Packet(v5Record(net.IPv4(10, 0, 0, 1), net.IPv4(1, 1, 1, 1))))
	frame[14+9] = 6 // IP protocol = TCP
	s := New([]byte("k"))
	if !s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("non-UDP IPv4 frame should be kept (with header rewritten)")
	}
	for _, ip := range [][]byte{{100, 64, 0, 7}, {9, 9, 9, 9}} {
		if bytes.Contains(frame[14:34], ip) { // within the IP header
			t.Errorf("non-UDP IPv4 header address %v not rewritten", net.IP(ip))
		}
	}
}

func TestSanitizeUnknownUDPDropped(t *testing.T) {
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{8, 8, 4, 4}, []byte{0xde, 0xad, 0xbe, 0xef, 0, 0})
	s := New([]byte("k"))
	if s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("non-flow UDP payload must be dropped")
	}
}

func v9OptTemplate(tid uint16) []byte {
	body := make([]byte, 6)
	be16(body[0:2], tid)
	be16(body[2:4], 4) // option scope length
	be16(body[4:6], 4) // option length
	body = append(body, 0, 1, 0, 4, 0, 8, 0, 4)
	fs := make([]byte, 4)
	be16(fs[0:2], 1)
	be16(fs[2:4], uint16(4+len(body)))
	return append(fs, body...)
}

func TestSanitizeOptionsDataDropped(t *testing.T) {
	rec := make([]byte, 8)
	copy(rec[4:8], net.IPv4(100, 64, 0, 7).To4()) // IE-8 value
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, v9Packet(1, v9OptTemplate(256), v9Data(256, rec)))
	s := New([]byte("k"))
	if s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("v9 options data must be dropped (could carry addresses)")
	}
	if bytes.Contains(frame, []byte{100, 64, 0, 7}) {
		// dropped frame is not written, but ensure Frame reported drop above
		t.Log("address present (expected: frame is dropped, not written)")
	}
}

func TestSanitizeIPv6IEDropped(t *testing.T) {
	fields := [][2]uint16{{8, 4}, {27, 16}} // IPv4 src + IPv6 src
	rec := make([]byte, 4+16)
	copy(rec[0:4], net.IPv4(100, 64, 0, 7).To4())
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, v9Packet(1, v9Template(256, fields), v9Data(256, rec)))
	s := New([]byte("k"))
	if s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("record with IPv6 IE must be dropped (IPv6 not rewritten in v0.4.1)")
	}
}

func TestSanitizeV9TemplateAndDataRewritten(t *testing.T) {
	fields := [][2]uint16{{8, 4}, {12, 4}, {4, 1}, {1, 4}} // srcIPv4, dstIPv4, proto, bytes
	rec := make([]byte, 13)
	copy(rec[0:4], net.IPv4(100, 64, 0, 7).To4())
	copy(rec[4:8], net.IPv4(9, 9, 9, 9).To4())
	rec[8] = 6
	be32(rec[9:13], 1500)
	payload := v9Packet(1, v9Template(256, fields), v9Data(256, rec))
	frame := ethFrame([4]byte{203, 0, 113, 5}, [4]byte{198, 51, 100, 10}, payload)

	s := New([]byte("k"))
	if !s.Frame(pcapio.LinkEthernet, frame) {
		t.Fatal("v9 template+data should be kept")
	}
	for _, ip := range [][]byte{{100, 64, 0, 7}, {9, 9, 9, 9}} {
		if bytes.Contains(frame, ip) {
			t.Errorf("v9 payload address %v leaked", net.IP(ip))
		}
	}
	// Sanitized payload still decodes via the real v9 decoder.
	info, _ := pcapio.ParseUDP(pcapio.LinkEthernet, frame)
	payOut := frame[info.PayloadOff : info.PayloadOff+info.PayloadLen]
	flows, err := netflow9.New(nil, nil).Decode(nil, payOut, net.IPv4(10, 0, 0, 1))
	if err != nil || len(flows) != 1 {
		t.Fatalf("v9 decode after sanitize: flows=%d err=%v", len(flows), err)
	}
	if flows[0].SrcIP.To4()[0] != 10 {
		t.Errorf("v9 payload src not remapped: %v", flows[0].SrcIP)
	}
}
