package netflow9

import (
	"encoding/binary"
	"net"
	"testing"
)

// A MikroTik flow template may export all four post-NAT fields without IE230.
// Keep wire direction intact: return traffic is destination translation, while
// the unchanged post-source remains a remote server, not the subscriber's NAT.
func TestMikroTikPostNATEndpoints(t *testing.T) {
	fields := []field{{8, 4}, {12, 4}, {7, 2}, {11, 2}, {225, 4}, {226, 4}, {227, 2}, {228, 2}}
	for _, tc := range []struct {
		name, src, dst, postSrc, postDst           string
		srcPort, dstPort, postSrcPort, postDstPort uint16
	}{
		{"source translation", "10.0.102.12", "198.51.100.3", "203.0.113.176", "198.51.100.3", 42286, 443, 52286, 443},
		{"destination translation", "57.144.140.3", "151.158.226.176", "57.144.140.3", "10.0.102.12", 443, 42286, 443, 42286},
		{"source port only", "203.0.113.1", "198.51.100.3", "203.0.113.1", "198.51.100.3", 1000, 443, 2000, 443},
		{"destination port only", "198.51.100.3", "203.0.113.1", "198.51.100.3", "203.0.113.1", 443, 2000, 443, 1000},
		{"unchanged zero ports", "198.51.100.3", "203.0.113.1", "198.51.100.3", "203.0.113.1", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := make([]byte, 24)
			copy(record[0:4], net.ParseIP(tc.src).To4())
			copy(record[4:8], net.ParseIP(tc.dst).To4())
			binary.BigEndian.PutUint16(record[8:10], tc.srcPort)
			binary.BigEndian.PutUint16(record[10:12], tc.dstPort)
			copy(record[12:16], net.ParseIP(tc.postSrc).To4())
			copy(record[16:20], net.ParseIP(tc.postDst).To4())
			binary.BigEndian.PutUint16(record[20:22], tc.postSrcPort)
			binary.BigEndian.PutUint16(record[22:24], tc.postDstPort)
			packet := append(v9Header(2, tUptime, tSecs, 1, 1), templateFlowSet(300, fields)...)
			packet = append(packet, dataFlowSet(300, record)...)
			flows, err := New(nil, nil).Decode(nil, packet, net.ParseIP("192.0.2.1"))
			if err != nil || len(flows) != 1 {
				t.Fatalf("decode: %d flows, %v", len(flows), err)
			}
			f := flows[0]
			if f.SrcIP.String() != tc.src || f.DstIP.String() != tc.dst || f.SrcPort != tc.srcPort || f.DstPort != tc.dstPort {
				t.Fatalf("original tuple changed: %+v", f)
			}
			if f.NatPublicIP.String() != tc.postSrc || f.NatPublicPort != tc.postSrcPort || f.NatDestIP.String() != tc.postDst || f.NatDestPort != tc.postDstPort {
				t.Fatalf("post-NAT tuple lost or swapped: %+v", f)
			}
			if f.NatEvent != 0 {
				t.Fatalf("invented a NAT event that was not exported: %d", f.NatEvent)
			}
			// Receive buffers are reused after Decode. Both post-NAT IPs must own
			// their bytes just as the original endpoints do.
			clear(packet)
			if f.NatDestIP.String() != tc.postDst || f.NatPublicIP.String() != tc.postSrc {
				t.Fatal("post-NAT addresses alias the receive buffer")
			}
		})
	}
}
