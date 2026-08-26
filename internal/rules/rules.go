// Package rules implements the configurable skip filters applied to every
// normalized flow before it is queued for insertion.
package rules

import (
	"net"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

const dnsPort = 53

// ReasonNoNAT labels a flow dropped for carrying no post-NAT address. It is
// reported per device by the compliance audit, so an exporter whose every flow
// is dropped this way is still visible as "sending, but not translating" rather
// than looking dead.
const ReasonNoNAT = "no_nat_translation"

// RuleSet holds the enabled skip rules. The zero value skips nothing.
type RuleSet struct {
	SkipDNS              bool
	SkipPrivateToPrivate bool
	SkipZeroBytes        bool
}

// New builds a RuleSet from the three configuration toggles.
func New(skipDNS, skipPrivateToPrivate, skipZeroBytes bool) *RuleSet {
	return &RuleSet{
		SkipDNS:              skipDNS,
		SkipPrivateToPrivate: skipPrivateToPrivate,
		SkipZeroBytes:        skipZeroBytes,
	}
}

// ShouldSkip reports whether rec must be dropped and, if so, a short reason
// label suitable for logging and metrics.
func (r *RuleSet) ShouldSkip(rec *normalizer.FlowRecord) (bool, string) {
	// Hard-coded and deliberately not configurable. This is an IPDR store, and
	// its records exist to answer exactly one question: which subscriber held
	// public IP X, port P, at time T. A flow with no post-NAT address cannot
	// answer it, no matter how well-formed it otherwise is — it only consumes
	// disk and retention budget. Checked first so the drop reason names the
	// real disqualifier rather than an incidental one.
	//
	// This drops 100% of NetFlow v5: that version carries no post-NAT fields at
	// all. That is the correct outcome here, and the compliance audit reports it
	// per device so it can never be mistaken for a silent exporter.
	if isUnsetIP(rec.NatPublicIP) {
		return true, ReasonNoNAT
	}
	if r.SkipZeroBytes && rec.Bytes == 0 {
		return true, "zero_bytes"
	}
	if r.SkipDNS && (rec.SrcPort == dnsPort || rec.DstPort == dnsPort) {
		return true, "dns"
	}
	if r.SkipPrivateToPrivate && isPrivate(rec.SrcIP) && isPrivate(rec.DstIP) {
		return true, "private_to_private"
	}
	return false, ""
}

// isUnsetIP reports whether an address was never populated: absent, or the
// unspecified address (0.0.0.0 / ::) that exporters emit for a field they do
// not fill in.
func isUnsetIP(ip net.IP) bool {
	return len(ip) == 0 || ip.IsUnspecified()
}

// isPrivate reports whether ip is non-globally-routable: RFC1918 private space,
// RFC6598 CGNAT (100.64.0.0/10), loopback, and link-local.
func isPrivate(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	if ip.IsPrivate() { // RFC1918 (v4) + RFC4193 (v6)
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 0x40 {
		return true // 100.64.0.0/10
	}
	return false
}
