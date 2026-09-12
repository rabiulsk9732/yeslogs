package hostmon

import (
	"strings"
	"testing"
)

func TestUDPReceiveErrors(t *testing.T) {
	in := "Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors\nUdp: 10 2 3 11 47 0\n"
	got, err := UDPReceiveErrors(strings.NewReader(in))
	if err != nil || got != 47 {
		t.Fatalf("got %d, %v", got, err)
	}
}

func TestDiskUsage(t *testing.T) {
	got, err := DiskUsage(t.TempDir())
	if err != nil || got < 0 || got > 100 {
		t.Fatalf("got %.2f, %v", got, err)
	}
}
