package pcapio

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestWriteReadRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w, err := NewWriter(&buf, LinkEthernet)
	if err != nil {
		t.Fatal(err)
	}
	frame := EthernetIPv4UDP([4]byte{10, 0, 0, 1}, [4]byte{8, 8, 8, 8}, 12345, 2055, []byte("hello"))
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := w.Write(Packet{Time: now, Data: frame}); err != nil {
		t.Fatal(err)
	}

	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if r.LinkType() != LinkEthernet {
		t.Fatalf("linktype = %d", r.LinkType())
	}
	p, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.Data, frame) {
		t.Error("frame round-trip mismatch")
	}
	if !p.Time.Equal(now) {
		t.Errorf("time = %v want %v", p.Time, now)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("want EOF, got %v", err)
	}
}

func TestParseUDP(t *testing.T) {
	frame := EthernetIPv4UDP([4]byte{192, 0, 2, 1}, [4]byte{198, 51, 100, 2}, 40000, 9995, []byte("PAYLOAD"))
	info, ok := ParseUDP(LinkEthernet, frame)
	if !ok {
		t.Fatal("ParseUDP failed on valid frame")
	}
	if info.DstPort != 9995 || info.SrcPort != 40000 {
		t.Errorf("ports = %d/%d", info.SrcPort, info.DstPort)
	}
	if got := string(frame[info.PayloadOff : info.PayloadOff+info.PayloadLen]); got != "PAYLOAD" {
		t.Errorf("payload = %q", got)
	}
	if info.L3Off != 14 || info.IHL != 20 {
		t.Errorf("offsets l3=%d ihl=%d", info.L3Off, info.IHL)
	}
}

func TestParseUDPRejectsNonUDP(t *testing.T) {
	if _, ok := ParseUDP(LinkEthernet, []byte{0, 1, 2}); ok {
		t.Error("short frame should not parse")
	}
}
