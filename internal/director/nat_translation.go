package director

import (
	"context"
	"net"
	"strconv"
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

func (r *FlowReader) hasIPv6(ctx context.Context) bool {
	if r.ipv6.Load() {
		return true
	}
	var n uint64
	err := r.conn.QueryRow(ctx, `SELECT count() FROM system.columns WHERE database = ? AND table = 'flow_logs' AND name IN ('src_ip_v6', 'dst_ip_v6', 'nat_public_ip_v6', 'nat_dest_ip_v6', 'exporter_ip_v6')`, r.db).Scan(&n)
	if err == nil && n == 5 {
		r.ipv6.Store(true)
		return true
	}
	return false
}

func hotDedupKey(hasDestination, hasIPv6 bool) string {
	if hasIPv6 {
		return dedupKey
	}
	if hasDestination {
		return destinationDedupKey
	}
	return legacyDedupKey
}

func parseCIDRRange(s string) (string, string, bool) {
	if !strings.Contains(s, "/") {
		return "", "", false
	}
	_, ipNet, err := net.ParseCIDR(strings.TrimSpace(s))
	if err != nil || ipNet == nil {
		return "", "", false
	}
	ip := ipNet.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	mask := ipNet.Mask
	if len(mask) != len(ip) {
		return "", "", false
	}
	start := make(net.IP, len(ip))
	end := make(net.IP, len(ip))
	for i := range ip {
		start[i] = ip[i] & mask[i]
		end[i] = ip[i] | ^mask[i]
	}
	return start.String(), end.String(), true
}

func parseIPList(s string) []string {
	if !strings.Contains(s, ",") {
		return nil
	}
	parts := strings.Split(s, ",")
	var ips []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if net.ParseIP(p) != nil {
			ips = append(ips, p)
		}
	}
	return ips
}

func usesIPv6(s string) bool {
	for _, part := range strings.Split(s, ",") {
		first := strings.TrimSpace(strings.Split(part, "/")[0])
		if ip := net.ParseIP(first); ip != nil && ip.To4() == nil {
			return true
		}
	}
	return false
}

func parsePortRange(s string) (uint16, uint16, bool) {
	if !strings.Contains(s, "-") {
		return 0, 0, false
	}
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	p1, err1 := strconv.ParseUint(strings.TrimSpace(parts[0]), 10, 16)
	p2, err2 := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 16)
	if err1 != nil || err2 != nil || p1 == 0 || p2 == 0 || p1 > p2 {
		return 0, 0, false
	}
	return uint16(p1), uint16(p2), true
}

// The requested translated/source-NAT columns describe the source tuple.
// Raw source/destination predicates preserve the exporter direction.
func addNATFilters(f SearchFilter, hot bool, conds *[]string, args *[]any) {
	// During a rolling migration an older table cannot contain IPv6 evidence.
	// Avoid referring to absent shadow columns or binding an IPv6 string to an
	// IPv4 parameter; the correct result until migration lands is no matches.
	if hot && !f.ipv6Available {
		if usesIPv6(f.PublicIP) {
			*conds = append(*conds, "0")
			f.PublicIP, f.PublicPort, f.PortRange = "", 0, ""
		}
		if usesIPv6(f.PrivateIP) {
			*conds = append(*conds, "0")
			f.PrivateIP = ""
		}
		if usesIPv6(f.DestIP) {
			*conds = append(*conds, "0")
			f.DestIP = ""
		}
	}
	baseParam, baseZero := "?", "'0.0.0.0'"
	if hot {
		baseParam, baseZero = "toIPv4(?)", "toIPv4('0.0.0.0')"
	}
	add := func(s string, a any) { *conds = append(*conds, s); *args = append(*args, a) }
	if f.PublicIP != "" || f.PublicPort > 0 || f.PortRange != "" {
		publicCol, sourceCol := "nat_public_ip", "src_ip"
		ipParam, zero := baseParam, baseZero
		if hot && f.ipv6Available && usesIPv6(f.PublicIP) {
			publicCol, sourceCol, ipParam, zero = "nat_public_ip_v6", "src_ip_v6", "toIPv6(?)", "toIPv6('::')"
		}
		branch := func(ip, port, changed string) string {
			terms := []string{changed}
			if f.PublicIP != "" {
				if start, end, ok := parseCIDRRange(f.PublicIP); ok {
					terms = append(terms, ip+" >= "+ipParam+" AND "+ip+" <= "+ipParam)
					*args = append(*args, start, end)
				} else if ips := parseIPList(f.PublicIP); len(ips) > 0 {
					placeholders := make([]string, len(ips))
					for i, oneIP := range ips {
						placeholders[i] = ipParam
						*args = append(*args, oneIP)
					}
					terms = append(terms, ip+" IN ("+strings.Join(placeholders, ", ")+")")
				} else {
					terms = append(terms, ip+" = "+ipParam)
					*args = append(*args, f.PublicIP)
				}
			}
			if pStart, pEnd, ok := parsePortRange(f.PortRange); ok {
				terms = append(terms, port+" >= ? AND "+port+" <= ?")
				*args = append(*args, pStart, pEnd)
			} else if f.PublicPort > 0 {
				terms = append(terms, port+" = ?")
				*args = append(*args, uint16(f.PublicPort))
			}
			return "(" + strings.Join(terms, " AND ") + ")"
		}
		source := publicCol + " != " + zero + " AND (" + publicCol + " != " + sourceCol + " OR nat_public_port != src_port)"
		if !hot {
			source = "nat_public_ip != '' AND " + source
		}
		match := branch(publicCol, "nat_public_port", source)

		*conds = append(*conds, match)
	}
	if f.PrivateIP != "" {
		col := "src_ip"
		ipParam := baseParam
		if hot && f.ipv6Available && usesIPv6(f.PrivateIP) {
			col, ipParam = "src_ip_v6", "toIPv6(?)"
		}
		if start, end, ok := parseCIDRRange(f.PrivateIP); ok {
			add(col+" >= "+ipParam+" AND "+col+" <= "+ipParam, start)
			*args = append(*args, end)
		} else if ips := parseIPList(f.PrivateIP); len(ips) > 0 {
			placeholders := make([]string, len(ips))
			for i, oneIP := range ips {
				placeholders[i] = ipParam
				*args = append(*args, oneIP)
			}
			*conds = append(*conds, col+" IN ("+strings.Join(placeholders, ", ")+")")
		} else {
			add(col+" = "+ipParam, f.PrivateIP)
		}
	}
	if f.DestIP != "" {
		col := "dst_ip"
		ipParam := baseParam
		if hot && f.ipv6Available && usesIPv6(f.DestIP) {
			col, ipParam = "dst_ip_v6", "toIPv6(?)"
		}
		if start, end, ok := parseCIDRRange(f.DestIP); ok {
			add(col+" >= "+ipParam+" AND "+col+" <= "+ipParam, start)
			*args = append(*args, end)
		} else if ips := parseIPList(f.DestIP); len(ips) > 0 {
			placeholders := make([]string, len(ips))
			for i, oneIP := range ips {
				placeholders[i] = ipParam
				*args = append(*args, oneIP)
			}
			*conds = append(*conds, col+" IN ("+strings.Join(placeholders, ", ")+")")
		} else {
			add(col+" = "+ipParam, f.DestIP)
		}
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
