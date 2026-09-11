package ipfix

import (
	"net"
	"testing"
)

func TestPostNATDestinationPreservesWireDirection(t *testing.T) {
	specs := []spec{{ie: 8, length: 4}, {ie: 12, length: 4}, {ie: 7, length: 2}, {ie: 11, length: 2},
		{ie: 225, length: 4}, {ie: 226, length: 4}, {ie: 227, length: 2}, {ie: 228, length: 2}}
	for _, tc := range []struct {
		name, src, dst, postSrc, postDst           string
		srcPort, dstPort, postSrcPort, postDstPort uint16
	}{
		{"SNAT", "10.0.102.12", "198.51.100.3", "203.0.113.176", "198.51.100.3", 42286, 443, 52286, 443},
		{"DNAT", "57.144.140.3", "151.158.226.176", "57.144.140.3", "10.0.102.12", 443, 42286, 443, 42286},
		{"source port only", "203.0.113.1", "198.51.100.3", "203.0.113.1", "198.51.100.3", 1000, 443, 2000, 443},
		{"destination port only", "198.51.100.3", "203.0.113.1", "198.51.100.3", "203.0.113.1", 443, 2000, 443, 1000},
		{"zero ports", "198.51.100.3", "203.0.113.1", "198.51.100.3", "203.0.113.1", 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var record []byte
			record = append(record, net.ParseIP(tc.src).To4()...)
			record = append(record, net.ParseIP(tc.dst).To4()...)
			record = append(record, u16b(tc.srcPort)...)
			record = append(record, u16b(tc.dstPort)...)
			record = append(record, net.ParseIP(tc.postSrc).To4()...)
			record = append(record, net.ParseIP(tc.postDst).To4()...)
			record = append(record, u16b(tc.postSrcPort)...)
			record = append(record, u16b(tc.postDstPort)...)
			msg := ipfixMsg(exSecs, 1, set(setTemplate, templateRecord(300, specs)), set(300, record))
			flows, err := New(nil, nil).Decode(nil, msg, net.ParseIP("192.0.2.1"))
			if err != nil || len(flows) != 1 {
				t.Fatalf("decode: %d flows, %v", len(flows), err)
			}
			f := flows[0]
			if f.SrcIP.String() != tc.src || f.DstIP.String() != tc.dst || f.SrcPort != tc.srcPort || f.DstPort != tc.dstPort {
				t.Fatalf("original tuple changed: %+v", f)
			}
			if f.NatPublicIP.String() != tc.postSrc || f.NatPublicPort != tc.postSrcPort || f.NatDestIP.String() != tc.postDst || f.NatDestPort != tc.postDstPort || f.NatEvent != 0 {
				t.Fatalf("post-NAT tuple lost, swapped or assigned an event: %+v", f)
			}
			clear(msg)
			if f.NatDestIP.String() != tc.postDst || f.NatPublicIP.String() != tc.postSrc {
				t.Fatal("post-NAT addresses alias the receive buffer")
			}
		})
	}
}
