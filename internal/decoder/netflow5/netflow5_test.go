package netflow5

import (
	"encoding/binary"
	"net"
	"testing"
)

// record builds one 48-byte NetFlow v5 record.
func record(src, dst net.IP, srcPort, dstPort uint16, pkts, octets, first, last uint32, proto uint8) []byte {
	r := make([]byte, recordLen)
	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint32(r[16:20], pkts)
	binary.BigEndian.PutUint32(r[20:24], octets)
	binary.BigEndian.PutUint32(r[24:28], first)
	binary.BigEndian.PutUint32(r[28:32], last)
	binary.BigEndian.PutUint16(r[32:34], srcPort)
	binary.BigEndian.PutUint16(r[34:36], dstPort)
	r[37] = 0x10 // tcp flags
	r[38] = proto
	return r
}

// packet builds a v5 datagram with the given declared count and records.
func packet(count int, sysUptime, unixSecs, unixNsecs uint32, recs ...[]byte) []byte {
	hdr := make([]byte, headerLen)
	binary.BigEndian.PutUint16(hdr[0:2], version)
	binary.BigEndian.PutUint16(hdr[2:4], uint16(count))
	binary.BigEndian.PutUint32(hdr[4:8], sysUptime)
	binary.BigEndian.PutUint32(hdr[8:12], unixSecs)
	binary.BigEndian.PutUint32(hdr[12:16], unixNsecs)
	pkt := hdr
	for _, r := range recs {
		pkt = append(pkt, r...)
	}
	return pkt
}

func TestDecodeBasic(t *testing.T) {
	d := New()
	rec := record(net.IPv4(10, 0, 0, 1), net.IPv4(8, 8, 8, 8), 1234, 53, 5, 600, 1000, 2000, 17)
	pkt := packet(1, 5000, 1_600_000_000, 0, rec)

	flows, err := d.Decode(nil, pkt, net.IPv4(192, 168, 1, 1))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("want 1 flow, got %d", len(flows))
	}
	f := flows[0]
	if !f.SrcIP.Equal(net.IPv4(10, 0, 0, 1)) {
		t.Errorf("srcIP = %v", f.SrcIP)
	}
	if !f.DstIP.Equal(net.IPv4(8, 8, 8, 8)) {
		t.Errorf("dstIP = %v", f.DstIP)
	}
	if f.SrcPort != 1234 || f.DstPort != 53 {
		t.Errorf("ports = %d/%d", f.SrcPort, f.DstPort)
	}
	if f.Bytes != 600 || f.Packets != 5 {
		t.Errorf("bytes/pkts = %d/%d", f.Bytes, f.Packets)
	}
	if f.Protocol != 17 {
		t.Errorf("proto = %d", f.Protocol)
	}
	if !f.ExporterIP.Equal(net.IPv4(192, 168, 1, 1)) {
		t.Errorf("exporter = %v", f.ExporterIP)
	}
	// boot = unixSecs*1000 - sysUptime; start = boot + first, end = boot + last.
	wantStart := int64(1_600_000_000)*1000 - 5000 + 1000
	if got := f.FlowStart.UnixMilli(); got != wantStart {
		t.Errorf("flowStart ms = %d, want %d", got, wantStart)
	}
	wantEnd := int64(1_600_000_000)*1000 - 5000 + 2000
	if got := f.FlowEnd.UnixMilli(); got != wantEnd {
		t.Errorf("flowEnd ms = %d, want %d", got, wantEnd)
	}
}

func TestDecodeAppendsToDst(t *testing.T) {
	d := New()
	rec := record(net.IPv4(1, 1, 1, 1), net.IPv4(2, 2, 2, 2), 1, 2, 1, 100, 0, 0, 6)
	existing, _ := d.Decode(nil, packet(1, 1, 1_600_000_000, 0, rec), nil)
	got, err := d.Decode(existing, packet(1, 1, 1_600_000_000, 0, rec), nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 flows after append, got %d", len(got))
	}
}

func TestDecodeShortPacket(t *testing.T) {
	d := New()
	if _, err := d.Decode(nil, []byte{0, 5, 0, 1}, nil); err == nil {
		t.Fatal("want error for short packet")
	}
}

func TestDecodeWrongVersion(t *testing.T) {
	d := New()
	pkt := make([]byte, headerLen)
	binary.BigEndian.PutUint16(pkt[0:2], 9)
	if _, err := d.Decode(nil, pkt, nil); err == nil {
		t.Fatal("want error for wrong version")
	}
}

func TestDecodeCountClampedToPayload(t *testing.T) {
	// Header claims 5 records but only 1 is present; must not over-read.
	d := New()
	rec := record(net.IPv4(1, 1, 1, 1), net.IPv4(2, 2, 2, 2), 1, 2, 1, 100, 0, 0, 6)
	pkt := packet(5, 1000, 1_600_000_000, 0, rec)
	flows, err := d.Decode(nil, pkt, nil)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("want 1 flow (clamped), got %d", len(flows))
	}
}
