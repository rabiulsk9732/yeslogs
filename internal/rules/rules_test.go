package rules

import (
	"net"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

func rec(src, dst string, sp, dp uint16, bytes uint64) normalizer.FlowRecord {
	return normalizer.FlowRecord{
		SrcIP:   net.ParseIP(src),
		DstIP:   net.ParseIP(dst),
		SrcPort: sp,
		DstPort: dp,
		Bytes:   bytes,
	}
}

func TestShouldSkip(t *testing.T) {
	rs := New(true, true, true)
	cases := []struct {
		name string
		r    normalizer.FlowRecord
		skip bool
		why  string
	}{
		{"dns dst", rec("10.0.0.1", "8.8.8.8", 33333, 53, 100), true, "dns"},
		{"dns src", rec("8.8.8.8", "10.0.0.1", 53, 33333, 100), true, "dns"},
		{"zero bytes", rec("10.0.0.1", "8.8.8.8", 1, 2, 0), true, "zero_bytes"},
		{"priv to priv", rec("10.0.0.1", "192.168.1.5", 1, 2, 100), true, "private_to_private"},
		{"cgnat to priv", rec("100.64.0.1", "10.0.0.1", 1, 2, 100), true, "private_to_private"},
		{"loopback to priv", rec("127.0.0.1", "10.0.0.1", 1, 2, 100), true, "private_to_private"},
		{"normal egress", rec("10.0.0.1", "8.8.8.8", 1, 2, 100), false, ""},
		{"public to public", rec("1.1.1.1", "8.8.8.8", 1, 2, 100), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skip, why := rs.ShouldSkip(&c.r)
			if skip != c.skip || (skip && why != c.why) {
				t.Errorf("ShouldSkip = (%v, %q), want (%v, %q)", skip, why, c.skip, c.why)
			}
		})
	}
}

func TestDisabledRulesSkipNothing(t *testing.T) {
	rs := New(false, false, false)
	r := rec("10.0.0.1", "192.168.1.1", 53, 53, 0)
	if skip, _ := rs.ShouldSkip(&r); skip {
		t.Error("no rules enabled, should not skip")
	}
}
