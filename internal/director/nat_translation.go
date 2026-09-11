package director

import (
	"context"
	"net"
	"strings"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// Missing fields in historical rows are unknown evidence, not unchanged tuples.
func recordedIP(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && !p.IsUnspecified()
}

func (r *natRecord) setNATTranslation() {
	if !recordedIP(r.PubIP) {
		r.PubIP, r.PubPort = "", 0
	}
	if !recordedIP(r.PostDstIP) {
		r.PostDstIP, r.PostDstPort = "", 0
	}
	r.PostSrcIP, r.PostSrcPort = r.PubIP, r.PubPort
	r.SourceKnown, r.DestinationKnown = r.PostSrcIP != "", r.PostDstIP != ""
	srcChanged := r.SourceKnown && (r.PrivIP != r.PostSrcIP || r.PrivPort != r.PostSrcPort)
	dstChanged := r.DestinationKnown && (r.DstIP != r.PostDstIP || r.DstPort != r.PostDstPort)
	r.NatIP, r.NatPort, r.Untranslated = "", 0, false
	switch {
	case srcChanged && dstChanged:
		r.Translation = "both"
	case srcChanged:
		r.Translation, r.NatIP, r.NatPort = "source", r.PostSrcIP, r.PostSrcPort
	case dstChanged:
		r.Translation, r.NatIP, r.NatPort = "destination", r.DstIP, r.DstPort
	case r.SourceKnown && r.DestinationKnown:
		r.Translation, r.Untranslated = "none", true
	default:
		r.Translation = "unknown"
	}
}

// Cache only a positive schema observation, so a separate reader can run before
// the additive collector migration and start reading new fields once it lands.
func (r *FlowReader) hasDestinationNAT(ctx context.Context) bool {
	if r.destinationNAT.Load() {
		return true
	}
	var n uint64
	err := r.conn.QueryRow(ctx, `SELECT count() FROM system.columns WHERE database = ? AND table = 'flow_logs' AND name IN ('nat_dest_ip', 'nat_dest_port')`, r.db).Scan(&n)
	if err == nil && n == 2 {
		r.destinationNAT.Store(true)
		return true
	}
	return false
}

func hotDedupKey(hasDestination bool) string {
	if hasDestination {
		return dedupKey
	}
	return legacyDedupKey
}

// Match the public tuple on the side whose translation is actually recorded.
// IP and port predicates are kept in the same branch to avoid cross-side hits.
func addNATFilters(f SearchFilter, hot bool, conds *[]string, args *[]any) {
	ipParam, zero := "?", "'0.0.0.0'"
	if hot {
		ipParam, zero = "toIPv4(?)", "toIPv4('0.0.0.0')"
	}
	add := func(s string, a any) { *conds = append(*conds, s); *args = append(*args, a) }
	if f.PublicIP != "" || f.PublicPort > 0 {
		branch := func(ip, port, changed string) string {
			terms := []string{changed}
			if f.PublicIP != "" {
				terms = append(terms, ip+" = "+ipParam)
				*args = append(*args, f.PublicIP)
			}
			if f.PublicPort > 0 {
				terms = append(terms, port+" = ?")
				*args = append(*args, uint16(f.PublicPort))
			}
			return "(" + strings.Join(terms, " AND ") + ")"
		}
		source := "nat_public_ip != " + zero + " AND (nat_public_ip != src_ip OR nat_public_port != src_port)"
		if !hot {
			source = "nat_public_ip != '' AND " + source
		}
		match := branch("nat_public_ip", "nat_public_port", source)
		if f.destinationNATAvailable {
			destination := "nat_dest_ip != " + zero + " AND (nat_dest_ip != dst_ip OR nat_dest_port != dst_port)"
			if !hot {
				destination = "nat_dest_ip != '' AND " + destination
			}
			match = "(" + match + " OR " + branch("dst_ip", "dst_port", destination) + ")"
		}
		*conds = append(*conds, match)
	}
	if f.PrivateIP != "" {
		if f.destinationNATAvailable {
			*conds = append(*conds, "(src_ip = "+ipParam+" OR nat_dest_ip = "+ipParam+")")
			*args = append(*args, f.PrivateIP, f.PrivateIP)
		} else {
			add("src_ip = "+ipParam, f.PrivateIP)
		}
	}
	if f.DestIP != "" {
		add("dst_ip = "+ipParam, f.DestIP)
	}
}

func resolveRecordDevice(devices []store.Device, scopeISP uint32, r natRecord) (store.Device, bool) {
	isp := r.ISPID
	if isp == 0 {
		isp = scopeISP
	}
	var found store.Device
	matches := 0
	for _, d := range devices {
		if (isp != 0 && d.ISPID != isp) || d.DeviceID != r.DevID {
			continue
		}
		if r.ExporterIP != "" && d.ExporterIP != r.ExporterIP {
			continue
		}
		found, matches = d, matches+1
	}
	return found, matches == 1
}
