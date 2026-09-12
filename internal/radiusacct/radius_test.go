package radiusacct

import (
	"crypto/md5"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestAccountingStart(t *testing.T) {
	secret := "shared-secret"
	p := make([]byte, 20)
	p[0] = 4
	p[1] = 7
	attr := func(typ byte, v []byte) { p = append(p, typ, byte(len(v)+2)); p = append(p, v...) }
	attr(1, []byte("alice"))
	attr(8, []byte{10, 0, 0, 9})
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, 1)
	attr(40, b)
	attr(44, []byte("s1"))
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	x := append([]byte(nil), p...)
	x = append(x, []byte(secret)...)
	sum := md5.Sum(x)
	copy(p[4:20], sum[:])
	c := NewCache(time.Hour)
	s := &Server{secret: []byte(secret), cache: c, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if _, e := s.handle(p, net.ParseIP("192.0.2.1")); e != nil {
		t.Fatal(e)
	}
	if got := c.Lookup(net.ParseIP("10.0.0.9")); got != "alice" {
		t.Fatalf("got %q", got)
	}
}
