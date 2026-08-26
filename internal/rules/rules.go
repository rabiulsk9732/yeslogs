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
	// Everything still here carries a post-NAT address, so from this point on
	// the configurable rules are deciding the fate of translation records only.
	// They were written for traffic flows and two of them are actively wrong
	// applied to a NAT event.
	//
	// Zero bytes is the worst of them. A NAT event record — the standard
	// separate-template form that Cisco, Juniper, Nokia and this fleet's DandyBNG
	// all emit — carries the translation, the ports, natEvent and often the
	// username, and NO byte counter, because it records an allocation rather than
	// traffic. Dropping it as an empty husk discards the single most valuable
	// record type an IPDR store can receive, while keeping the traffic flows that
	// cannot answer anything.
	//
	// Found live on 2026-08-26: exporter 103.204.1.14 sends template 265 with
	// IE 225/226/227/228/230 + username. Every one of those records was being
	// discarded here, and the device was being reported to its owner as "not
	// logging NAT" when it was doing exactly the right thing.
	if r.SkipZeroBytes && rec.Bytes == 0 && !isTranslation(rec) {
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

// isTranslation reports whether rec is a NAT translation record rather than a
// traffic flow that merely happens to carry a post-NAT address.
//
// The post-NAT port is the discriminator. Under CGNAT one public IP is shared by
// many subscribers, so the port is what identifies one — a record that has it is
// answering the question this store exists for, whatever its byte counter says.
func isTranslation(rec *normalizer.FlowRecord) bool {
	return rec.NatPublicPort != 0
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
