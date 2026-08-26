package rules

import (
	"net"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

// rec builds a translated flow — one that carries a post-NAT address, and so
// survives the hard-coded NAT rule and actually exercises the rule under test.
func rec(src, dst string, sp, dp uint16, bytes uint64) normalizer.FlowRecord {
	r := untranslated(src, dst, sp, dp, bytes)
	r.NatPublicIP = net.ParseIP("203.0.113.7")
	r.NatPublicPort = 40000
	return r
}

// untranslated builds a flow with no post-NAT address, as an exporter that logs
// traffic rather than translations emits.
func untranslated(src, dst string, sp, dp uint16, bytes uint64) normalizer.FlowRecord {
	return normalizer.FlowRecord{
		SrcIP:   net.ParseIP(src),
		DstIP:   net.ParseIP(dst),
		SrcPort: sp,
		DstPort: dp,
		Bytes:   bytes,
	}
}

// husk builds a zero-byte traffic flow: it has a post-NAT address but no
// post-NAT port, so it is not a translation record and the zero-bytes rule still
// applies to it.
func husk(src, dst string, sp, dp uint16) normalizer.FlowRecord {
	r := untranslated(src, dst, sp, dp, 0)
	r.NatPublicIP = net.ParseIP("203.0.113.7")
	return r
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
		{"zero bytes", husk("10.0.0.1", "8.8.8.8", 1, 2), true, "zero_bytes"},
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

// The NAT rule is not part of RuleSet and cannot be switched off: an IPDR store
// has no use for a flow that cannot answer a lawful request.
func TestNoNATRuleIsUnconditional(t *testing.T) {
	cases := []struct {
		name string
		ip   net.IP
	}{
		{"field absent", nil},
		{"empty", net.IP{}},
		{"zero v4", net.ParseIP("0.0.0.0")},
		{"zero v6", net.ParseIP("::")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, rs := range []*RuleSet{New(false, false, false), New(true, true, true)} {
				r := untranslated("10.0.0.1", "8.8.8.8", 1, 2, 100)
				r.NatPublicIP = c.ip
				skip, why := rs.ShouldSkip(&r)
				if !skip || why != ReasonNoNAT {
					t.Errorf("ShouldSkip = (%v, %q), want (true, %q)", skip, why, ReasonNoNAT)
				}
			}
		})
	}
}

// The NAT rule is checked first so the reason names the real disqualifier: a
// flow with no translation is not IPDR data whether or not it is also DNS.
func TestNoNATRuleReportedAheadOfOtherReasons(t *testing.T) {
	rs := New(true, true, true)
	r := untranslated("10.0.0.1", "192.168.1.1", 53, 53, 0) // DNS, zero-byte, priv→priv
	skip, why := rs.ShouldSkip(&r)
	if !skip || why != ReasonNoNAT {
		t.Errorf("ShouldSkip = (%v, %q), want (true, %q)", skip, why, ReasonNoNAT)
	}
}

// A translated flow with every configurable rule off must pass — the NAT rule
// must not become a blanket filter.
func TestDisabledRulesSkipNothingTranslated(t *testing.T) {
	rs := New(false, false, false)
	r := rec("10.0.0.1", "192.168.1.1", 53, 53, 0)
	if skip, why := rs.ShouldSkip(&r); skip {
		t.Errorf("no configurable rules enabled and the flow is translated; should not skip (got %q)", why)
	}
}

// A post-NAT address equal to the source is not a translation, but it IS a
// populated field. Dropping it is a policy question the console surfaces
// (RequireNAT), not something this rule should decide for the operator.
func TestSameAsSourceIsNotDroppedHere(t *testing.T) {
	rs := New(false, false, false)
	r := untranslated("1.1.1.1", "8.8.8.8", 1, 2, 100)
	r.NatPublicIP = net.ParseIP("1.1.1.1")
	if skip, why := rs.ShouldSkip(&r); skip {
		t.Errorf("untranslated-but-populated flow should survive the dataplane rule, got skip=%q", why)
	}
}

// natEvent builds a NAT event record as exporters emit them in a dedicated
// template: the translation, the ports, and no byte counter, because it records
// an allocation rather than traffic.
func natEvent(priv string, privPort uint16, pub string, pubPort uint16, dst string, dstPort uint16) normalizer.FlowRecord {
	return normalizer.FlowRecord{
		SrcIP: net.ParseIP(priv), SrcPort: privPort,
		DstIP: net.ParseIP(dst), DstPort: dstPort,
		NatPublicIP: net.ParseIP(pub), NatPublicPort: pubPort,
		Bytes: 0, Packets: 0,
	}
}

// The regression that mattered. Found live 2026-08-26: exporter 103.204.1.14
// sends NAT events in template 265 (IE 225/226/227/228/230 + username) with no
// byte counter. SkipZeroBytes discarded every one, so the store kept only the
// traffic flows that can answer nothing — and the device's owner was told it
// was not logging NAT while it was doing exactly the right thing.
func TestNATEventSurvivesZeroBytesRule(t *testing.T) {
	rs := New(true, true, true) // zero-bytes rule ON, as it is across the fleet
	r := natEvent("100.64.12.9", 51344, "103.204.1.14", 40112, "142.251.42.14", 443)
	if skip, why := rs.ShouldSkip(&r); skip {
		t.Fatalf("a NAT event record was dropped as %q; it carries the translation this store exists to record", why)
	}
}

// A traffic flow with no bytes and no post-NAT port is still an empty husk and
// the rule must still drop it — the exemption is for translations, not for
// everything that reaches it.
func TestZeroByteHuskStillDropped(t *testing.T) {
	rs := New(true, true, true)
	r := untranslated("10.0.0.1", "8.8.8.8", 1234, 4321, 0)
	r.NatPublicIP = net.ParseIP("203.0.113.7") // has an address, no port
	if skip, why := rs.ShouldSkip(&r); !skip || why != "zero_bytes" {
		t.Errorf("ShouldSkip = (%v, %q), want (true, \"zero_bytes\")", skip, why)
	}
}

// A NAT event still has to carry a post-NAT address; the exemption must not
// become a way for untranslated records to get in.
func TestZeroBytesExemptionStillRequiresATranslation(t *testing.T) {
	rs := New(true, true, true)
	r := untranslated("10.0.0.1", "8.8.8.8", 1234, 4321, 0)
	r.NatPublicPort = 40112 // port but no address
	if skip, why := rs.ShouldSkip(&r); !skip || why != ReasonNoNAT {
		t.Errorf("ShouldSkip = (%v, %q), want (true, %q)", skip, why, ReasonNoNAT)
	}
}
