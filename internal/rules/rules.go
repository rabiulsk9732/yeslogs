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
	// A record carrying a translation is kept unconditionally. Nothing below may
	// touch it.
	//
	// The rules that follow exist to reduce TRAFFIC-FLOW volume: chatty DNS,
	// intra-network hops, zero-byte husks. None of that reasoning transfers to a
	// subscriber mapping. This store's records are answers to lawful requests,
	// and a mapping discarded to save disk is an answer that cannot be given
	// later — flow export is fire-and-forget, so it is gone permanently.
	//
	// Each of the three was actively destroying evidence on this fleet:
	//
	//   zero bytes — a NAT event record carries the translation, the ports,
	//     natEvent and often the subscriber's username, and NO byte counter,
	//     because it records an allocation rather than traffic. Cisco, Juniper,
	//     Nokia and DandyBNG all emit them this way. Measured 2026-08-26:
	//     exporter 103.204.1.14 sends template 265 with IE 225/226/227/228/230
	//     plus username, and 100% of it was being dropped as an empty husk. The
	//     device was then graded "not logging NAT" and its owner told to fix a
	//     device that was already correct.
	//
	//   dns — a translation whose destination is port 53 is still a subscriber
	//     holding a public ip:port at a point in time. Measured on the one box
	//     where this rule is off, 24.8% of all NAT events involve port 53; on the
	//     three where it is on, that share of every mapping was being destroyed.
	//
	//   private to private — under CGNAT the subscriber side is RFC1918 or
	//     100.64/10 by definition. A translation between two private addresses is
	//     unusual but entirely real, and dropping it loses the mapping.
	if isTranslation(rec) {
		return false, ""
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

// isTranslation reports whether rec records an address translation rather than a
// traffic flow that merely happens to carry a post-NAT address.
//
// Two forms count, because two kinds of device produce them:
//
//   - a post-NAT port is present. Under CGNAT one public IP is shared by many
//     subscribers, so the port is what identifies one, and a record carrying it
//     answers the question this store exists for whatever its byte counter says.
//
//   - the post-NAT address simply differs from the source. 1:1 NAT and
//     deterministic NAT without PAT emit IE 225 with no IE 227 at all, and such a
//     record still answers "who held public IP X at time T". Keying only on the
//     port would discard every translation those devices produce — the same class
//     of mistake as treating a NAT event as an empty husk.
//
// A record whose post-NAT address equals its source translated nothing, so it is
// a traffic flow and the configurable rules may reduce it.
func isTranslation(rec *normalizer.FlowRecord) bool {
	if isUnsetIP(rec.NatPublicIP) {
		return false
	}
	if rec.NatPublicPort != 0 {
		return true
	}
	return !rec.NatPublicIP.Equal(rec.SrcIP)
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
