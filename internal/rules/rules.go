// Package rules implements the configurable skip filters applied to every
// normalized flow before it is queued for insertion.
package rules

import (
	"net"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

const dnsPort = 53

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
