package syslognat

import (
	"net"
	"testing"
)

func TestFortinet(t *testing.T) {
	d := New()
	got, e := d.Decode(nil, []byte(`<189>date=2026-09-12 srcip=10.0.0.4 srcport=1234 dstip=8.8.8.8 dstport=53 proto=udp transip=203.0.113.9 transport=40001 action=accept`), net.ParseIP("192.0.2.2"))
	if e != nil || len(got) != 1 || got[0].NatPublicPort != 40001 || got[0].Protocol != 17 {
		t.Fatalf("%+v %v", got, e)
	}
}
func TestCiscoASA(t *testing.T) {
	d := New()
	got, e := d.Decode(nil, []byte(`%ASA-6-305011: Built dynamic TCP translation from inside:10.0.0.8/2345 to outside:203.0.113.8/52345`), net.ParseIP("192.0.2.3"))
	if e != nil || len(got) != 1 || got[0].NatEvent != 1 {
		t.Fatalf("%+v %v", got, e)
	}
}
