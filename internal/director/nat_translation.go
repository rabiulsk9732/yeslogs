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
		r.Translation, r.NatIP, r.NatPort = "both", r.PostSrcIP, r.PostSrcPort
	case srcChanged:
		r.Translation, r.NatIP, r.NatPort = "source", r.PostSrcIP, r.PostSrcPort
	case dstChanged:
		r.Translation = "destination"
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

// The requested translated/source-NAT columns describe the source tuple.
// Raw source/destination predicates preserve the exporter direction.
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

		*conds = append(*conds, match)
	}
	if f.PrivateIP != "" {
		add("src_ip = "+ipParam, f.PrivateIP)
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
