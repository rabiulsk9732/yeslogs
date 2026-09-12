package director

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/console
var consoleFS embed.FS

// assetsHandler serves embedded static assets (vendored jQuery/Font Awesome) at
// /assets/ so the console works fully offline (no CDN).
func (s *Server) assetsHandler() http.Handler {
	sub, err := fs.Sub(consoleFS, "web/console")
	if err != nil {
		return http.NotFoundHandler()
	}
	return cachedAssets(sub, http.FileServerFS(sub))
}

// ---- console JSON DTOs (shapes the SPA consumes) ----

type widget struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
	Color string `json:"color"`
}
type infoBox struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Pct   string `json:"pct"`
	Note  string `json:"note"`
	Icon  string `json:"icon"`
	Color string `json:"color"`
}
type natRecord struct {
	Date       string    `json:"date"`
	Clock      string    `json:"clock"`
	Time       string    `json:"time"`
	StartTime  string    `json:"startTime"`
	EndTime    string    `json:"endTime"`
	EndAt      time.Time `json:"-"`
	At         time.Time `json:"-"`
	Sub        string    `json:"sub"`
	DevID      uint32    `json:"devId"`
	ISPID      uint32    `json:"ispId"`
	ExporterIP string    `json:"exporterIp"`
	PrivIP     string    `json:"privIp"`
	PrivPort   int       `json:"privPort"`
	PubIP      string    `json:"pubIp"`
	PubPort    int       `json:"pubPort"`
	Proto      string    `json:"proto"`
	Dest       string    `json:"dest"`
	DstIP      string    `json:"dstIp"`
	DstPort    int       `json:"dstPort"`
	Action     string    `json:"action"`
	// PubIP/PubPort retain the raw post-NAT source for API compatibility.
	// A mapping is inferred only from a changed tuple, never IP equality alone.
	PostSrcIP        string `json:"postSrcIp"`
	PostSrcPort      int    `json:"postSrcPort"`
	PostDstIP        string `json:"postDstIp"`
	PostDstPort      int    `json:"postDstPort"`
	NatIP            string `json:"natIp"`
	NatPort          int    `json:"natPort"`
	Translation      string `json:"translation"`
	SourceKnown      bool   `json:"sourceKnown"`
	DestinationKnown bool   `json:"destinationKnown"`
	Untranslated     bool   `json:"untranslated,omitempty"`
	// Username is the subscriber identity the exporter itself reported (IE 371).
	// When present it answers a lawful request outright — no CRM resolution and
	// no inference from an address that may have been reallocated since.
	Username string `json:"username,omitempty"`
	// NatEvent is 1 for an allocation and 2 for a release (IE 230). The pair
	// bounds when a mapping was actually held.
	NatEvent     uint8  `json:"natEvent,omitempty"`
	CRMReference string `json:"crmReference,omitempty"`
	CRMStatus    string `json:"crmStatus,omitempty"`
	CRMUsername  string `json:"crmUsername,omitempty"`
	CRMName      string `json:"crmName,omitempty"`
	CRMAddress   string `json:"crmAddress,omitempty"`
	CRMPhone     string `json:"crmPhone,omitempty"`
	CRMAccountID string `json:"crmAccountId,omitempty"`
	CRMSessionID string `json:"crmSessionId,omitempty"`
	CRMMAC       string `json:"crmMac,omitempty"`
}
type protoSlice struct {
	Name  string  `json:"name"`
	Pct   float64 `json:"pct"`
	Color string  `json:"color"`
}
type topSub struct {
	Label string `json:"label"`
	Value string `json:"value"`
	Pct   string `json:"pct"`
}
type consoleData struct {
	Widgets   []widget     `json:"widgets"`
	InfoBoxes []infoBox    `json:"infoBoxes"`
	Records   []natRecord  `json:"records"`
	Hourly    []uint64     `json:"hourly"`
	ProtoMix  []protoSlice `json:"protoMix"`
	Region    [][]any      `json:"region"`
	TopSubs   []topSub     `json:"topSubs"`
	Empty     bool         `json:"empty"`
}

func protoName(p uint8) string {
	switch p {
	case 6:
		return "TCP"
	case 17:
		return "UDP"
	case 1:
		return "ICMP"
	default:
		return "OTH"
	}
}

func humanBytes2(b uint64) string { return humanBytes(b) }

// ConsoleData assembles the dashboard/analytics payload from ClickHouse, scoped
// to ispID (0 = all, director only). Each section is best-effort.
// consoleDataRaw aggregates straight from flow_logs. Used only until the rollup
// is populated (fresh installs); see ConsoleData in rollup.go for the fast path.
func (r *FlowReader) consoleDataRaw(ctx context.Context, ispID uint32, days int) consoleData {
	var d consoleData
	where, args := scope(ispID, days)

	// The dashboard fires several independent aggregations over a large window.
	// Run them CONCURRENTLY (the ClickHouse conn is a pool) so total latency is
	// the slowest single query, not their sum. uniq (HyperLogLog, ~1.6% error)
	// keeps the distinct counts ~5x cheaper than uniqExact over 100M+ rows, and
	// the scalars/records/charts are combined into one aggregate scan each.
	var rows, subs, devs, totBytes, natIPs, translated, today uint64
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() {
		_ = r.conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(), uniq(src_ip), uniq(device_id), sum(bytes), uniq(nat_public_ip),
			        countIf(nat_public_ip != toIPv4('0.0.0.0') AND nat_public_ip != src_ip)
			 FROM %s.flow_logs WHERE %s`, r.db, where), args...).
			Scan(&rows, &subs, &devs, &totBytes, &natIPs, &translated)
	})
	run(func() {
		_ = r.conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count() FROM %s.flow_logs WHERE event_date = today()%s`, r.db, ispClause(ispID)), ispArgs(ispID)...).Scan(&today)
	})
	run(func() { d.Records = r.records(ctx, ispID, days, 50) }) // distinct struct fields: no shared write
	run(func() { d.Hourly = r.hourly(ctx, ispID) })
	run(func() { d.ProtoMix = r.protoMix(ctx, ispID, days) })
	run(func() { d.Region = r.regionByDevice(ctx, ispID, days) })
	run(func() { d.TopSubs = r.topSubsByBytes(ctx, ispID, days) })
	wg.Wait()

	d.Empty = rows == 0
	d.Widgets = []widget{
		{Value: group(rows), Label: "NAT Flows (window)", Icon: "fa-diagram-project", Color: "#0077b6"},
		{Value: group(today), Label: "Translations Today", Icon: "fa-right-left", Color: "#00a3c4"},
		{Value: group(subs), Label: "Subscribers Seen", Icon: "fa-users", Color: "#2a9d8f"},
		{Value: humanBytes2(totBytes), Label: "Logged Volume", Icon: "fa-shield-halved", Color: "#e76f51"},
	}
	poolPct := pctOf(natIPs, 256)
	d.InfoBoxes = []infoBox{
		{Label: "CGNAT Public IPs Seen", Value: group(natIPs), Pct: poolPct, Note: fmt.Sprintf("%s of /24 pool", poolPct), Icon: "fa-server", Color: "#0077b6"},
		{Label: "Active Devices", Value: group(devs), Pct: pctOf(devs, 50), Note: "exporters reporting", Icon: "fa-plug", Color: "#2a9d8f"},
		// Session duration used to sit here, computed as flow_end − flow_start.
		// Records are now stamped with the collector's clock rather than the
		// exporter's, so that difference is always zero and the tile would show a
		// permanent 0 — worse than showing nothing. What matters on an IPDR
		// dashboard anyway is not how long sessions ran but how much of what was
		// stored can actually answer a request.
		{Label: "Answerable Records", Value: group(translated), Pct: pctOf(translated, max(rows, 1)),
			Note: "carry a real NAT translation", Icon: "fa-scale-balanced", Color: "#e76f51"},
	}
	return d
}

func ispClause(ispID uint32) string {
	if ispID == 0 {
		return ""
	}
	return " AND isp_id = ?"
}
func ispArgs(ispID uint32) []any {
	if ispID == 0 {
		return nil
	}
	return []any{ispID}
}

func (r *FlowReader) records(ctx context.Context, ispID uint32, days, limit int) []natRecord {
	now := time.Now().In(istLoc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, istLoc).AddDate(0, 0, -days)
	rows, _ := r.Search(ctx, SearchFilter{ISPID: ispID, From: from, RequireNAT: true}, limit, 0)
	return rows
}

func group(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
func pctOf(n, of uint64) string {
	if of == 0 {
		return "0%"
	}
	p := n * 100 / of
	if p > 100 {
		p = 100
	}
	return fmt.Sprintf("%d%%", p)
}

func protoNum(s string) uint8 {
	switch strings.ToUpper(s) {
	case "TCP":
		return 6
	case "UDP":
		return 17
	case "ICMP":
		return 1
	}
	return 0
}

// SearchFilter is a flow-log query. At least one IP or device filter must be set.
type SearchFilter struct {
	ISPID                       uint32
	PublicIP, PrivateIP, DestIP string
	PublicPort                  int
	PortRange                   string // e.g. "20000-25000"
	Proto                       string
	DeviceID                    uint32
	ExporterIP                  string
	destinationNATAvailable     bool
	ipv6Available               bool
	// Username matches the subscriber identity the exporter reported (IE 371).
	// It is the strongest selector this store has: it needs no resolution of an
	// address that may have been reallocated since the time being asked about.
	Username string
	From, To time.Time
	// RequireNAT requires at least one recorded post-NAT address. It preserves
	// unchanged tuples and missing historical destination evidence for inspection.
	RequireNAT bool
}

// HasSelector reports whether the filter narrows the scan (an IP or device).
func (f SearchFilter) HasSelector() bool {
	return f.PublicIP != "" || f.PrivateIP != "" || f.DestIP != "" || f.DeviceID != 0 || f.ExporterIP != "" || f.Username != "" || f.PortRange != ""
}

// hotWhere builds the WHERE clause + bound args for the hot flow_logs table.
func hotWhere(f SearchFilter) (string, []any, bool) {
	var conds []string
	var args []any
	add := func(c string, a any) { conds = append(conds, c); args = append(args, a) }
	addNATFilters(f, true, &conds, &args)
	if pn := protoNum(f.Proto); pn > 0 {
		add("protocol = ?", pn)
	}
	if f.DeviceID > 0 {
		add("device_id = ?", f.DeviceID)
	}
	if f.ExporterIP != "" {
		if ip := net.ParseIP(f.ExporterIP); ip != nil && ip.To4() == nil {
			if f.ipv6Available {
				add("exporter_ip_v6 = toIPv6(?)", f.ExporterIP)
			} else {
				conds = append(conds, "0")
			}
		} else {
			add("exporter_ip = toIPv4(?)", f.ExporterIP)
		}
	}
	if f.Username != "" {
		add("username = ?", f.Username)
	}
	if f.RequireNAT {
		cond := "nat_public_ip != toIPv4('0.0.0.0')"
		if f.destinationNATAvailable {
			cond = "(" + cond + " OR nat_dest_ip != toIPv4('0.0.0.0'))"
		}
		if f.ipv6Available {
			cond = "(" + cond + " OR nat_public_ip_v6 != toIPv6('::') OR nat_dest_ip_v6 != toIPv6('::'))"
		}
		conds = append(conds, cond)
	}
	if !f.From.IsZero() {
		add("flow_start >= ?", f.From.UTC())
		// Also bound the partition key (event_date is the IST date of the flow) so
		// ClickHouse prunes whole day-partitions instead of scanning all of them
		// via weaker part min/max — this is what keeps dated search fast at TB scale.
		// 1-day margin absorbs any IST/UTC boundary skew; flow_start above is exact.
		add("event_date >= toDate(?)", f.From.In(istLoc).AddDate(0, 0, -1).Format("2006-01-02"))
	}
	if !f.To.IsZero() {
		add("flow_start <= ?", f.To.UTC())
		add("event_date <= toDate(?)", f.To.In(istLoc).AddDate(0, 0, 1).Format("2006-01-02"))
	}
	if f.ISPID != 0 {
		add("isp_id = ?", f.ISPID)
	}
	if len(conds) == 0 {
		return "", nil, false
	}
	return strings.Join(conds, " AND "), args, true
}

// countCap bounds how far SearchCount counts so a broad filter over millions of
// rows can't turn the total into an expensive full scan; beyond it we report N+.
const countCap = 100000

// dedupKey collapses evidence that was stored more than once. A router with two
// WAN uplinks and one traffic-flow target per uplink exports every flow twice:
// same translation, same timestamps, differing only in the UDP source address the
// copy arrived from (exporter_ip, and — before the two uplinks were merged onto
// one device_id — a second device_id). Both copies are kept on disk on purpose.
// Dropping one at ingest would be a skip rule on evidence, and this fleet has
// already lost records that way (see the 2026-08-26 skip-rule incident), so the
// collapse happens here instead: at the layer that answers lawful requests, where
// showing the same translation twice is its own kind of wrong.
//
// The list is deliberately WIDE — every evidence column EXCEPT exporter_ip and
// device_id, the only two that legitimately differ between copies of one flow.
// Anything differing in any other field survives as its own row, so an error in
// this list leaves a duplicate visible rather than hiding a distinct record.
// Never narrow it to "the 5-tuple": a NAT create and its matching delete can share
// one, and collapsing those would erase the end of a subscriber's translation.
const legacyDedupKey = "isp_id, flow_start, flow_end, src_ip, src_port, dst_ip, dst_port, " +
	"nat_public_ip, nat_public_port, protocol, bytes, packets, flow_type, nat_event, username"

const destinationDedupKey = legacyDedupKey + ", nat_dest_ip, nat_dest_port"
const dedupKey = destinationDedupKey + ", src_ip_v6, dst_ip_v6, nat_public_ip_v6, nat_dest_ip_v6"

// SearchCount returns the number of hot rows matching f, capped at countCap
// (a returned value == countCap means "countCap or more").
func (r *FlowReader) SearchCount(ctx context.Context, f SearchFilter) (uint64, error) {
	f.destinationNATAvailable = r.hasDestinationNAT(ctx)
	f.ipv6Available = r.hasIPv6(ctx)
	where, args, ok := hotWhere(f)
	if !ok {
		return 0, fmt.Errorf("no filter")
	}
	q := fmt.Sprintf(`SELECT count() FROM (SELECT 1 FROM %s.flow_logs WHERE %s LIMIT 1 BY %s LIMIT %d)`, r.db, where, hotDedupKey(f.destinationNATAvailable, f.ipv6Available), countCap)
	var n uint64
	err := r.conn.QueryRow(ctx, q, args...).Scan(&n)
	return n, err
}

// Search returns one page of flow-log records matching the filter (tenant-scoped
// by ISPID), ordered newest-first, using LIMIT/OFFSET server-side pagination so
// the client only ever holds a single page.
func (r *FlowReader) Search(ctx context.Context, f SearchFilter, limit, offset int) ([]natRecord, error) {
	f.destinationNATAvailable = r.hasDestinationNAT(ctx)
	f.ipv6Available = r.hasIPv6(ctx)
	where, args, ok := hotWhere(f)
	if !ok {
		return nil, fmt.Errorf("no filter")
	}
	exporter, source, public, destination := "exporter_ip", "src_ip", "nat_public_ip", "dst_ip"
	if f.ipv6Available {
		exporter = "if(exporter_ip_v6=toIPv6('::'),toIPv6(exporter_ip),exporter_ip_v6)"
		source = "if(src_ip_v6=toIPv6('::'),toIPv6(src_ip),src_ip_v6)"
		public = "if(nat_public_ip_v6=toIPv6('::'),toIPv6(nat_public_ip),nat_public_ip_v6)"
		destination = "if(dst_ip_v6=toIPv6('::'),toIPv6(dst_ip),dst_ip_v6)"
	}
	postDest := "toIPv4('0.0.0.0'), toUInt16(0)"
	if f.destinationNATAvailable {
		postDest = "nat_dest_ip, nat_dest_port"
		if f.ipv6Available {
			postDest = "if(nat_dest_ip_v6=toIPv6('::'),toIPv6(nat_dest_ip),nat_dest_ip_v6), nat_dest_port"
		}
	}
	q := fmt.Sprintf(`SELECT flow_start, flow_end, isp_id, device_id, %s, %s, src_port, %s, nat_public_port,
		%s, dst_port, %s, protocol, flow_type, username, nat_event
		FROM %s.flow_logs WHERE %s ORDER BY flow_start DESC, %s, exporter_ip, device_id LIMIT 1 BY %s LIMIT %d OFFSET %d`, exporter, source, public, destination, postDest, r.db, where, hotDedupKey(f.destinationNATAvailable, f.ipv6Available), hotDedupKey(f.destinationNATAvailable, f.ipv6Available), limit, offset)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make([]natRecord, 0, 128)
	for rs.Next() {
		var ts, end time.Time
		var rec natRecord
		var sip, pip, dip, pdip, exporter net.IP
		var sp, pp, dp, pdp uint16
		var pr uint8
		var ft string
		if err := rs.Scan(&ts, &end, &rec.ISPID, &rec.DevID, &exporter, &sip, &sp, &pip, &pp, &dip, &dp, &pdip, &pdp, &pr, &ft, &rec.Username, &rec.NatEvent); err != nil {
			return out, err
		}
		rec.At, rec.EndAt = ts.UTC(), end.UTC()
		rec.Date, rec.Clock, rec.Time = ts.In(istLoc).Format("2006-01-02"), ts.In(istLoc).Format("15:04:05"), ts.In(istLoc).Format("2006-01-02 15:04:05")
		rec.StartTime, rec.EndTime = rec.Time, end.In(istLoc).Format("2006-01-02 15:04:05")
		rec.Sub, rec.ExporterIP = fmt.Sprintf("DEV-%d", rec.DevID), exporter.String()
		rec.PrivIP, rec.PrivPort = sip.String(), int(sp)
		rec.PubIP, rec.PubPort = pip.String(), int(pp)
		rec.DstIP, rec.DstPort = dip.String(), int(dp)
		rec.PostDstIP, rec.PostDstPort = pdip.String(), int(pdp)
		rec.Proto, rec.Action = protoName(pr), strings.ToUpper(ft)
		rec.Dest = fmt.Sprintf("%s:%d", dip.String(), dp)
		rec.setNATTranslation()
		out = append(out, rec)
	}
	return out, rs.Err()
}

func (r *FlowReader) hourly(ctx context.Context, ispID uint32) []uint64 {
	q := fmt.Sprintf(`SELECT toHour(flow_start) h, count() c FROM %s.flow_logs
		WHERE event_date = today()%s GROUP BY h ORDER BY h`, r.db, ispClause(ispID))
	out := make([]uint64, 24)
	rs, err := r.conn.Query(ctx, q, ispArgs(ispID)...)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var h uint8
		var c uint64
		if rs.Scan(&h, &c) == nil && int(h) < 24 {
			out[h] = c
		}
	}
	return out
}

func (r *FlowReader) protoMix(ctx context.Context, ispID uint32, days int) []protoSlice {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT protocol, count() c FROM %s.flow_logs WHERE %s GROUP BY protocol ORDER BY c DESC`, r.db, where)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	agg := map[string]uint64{}
	var sum uint64
	for rs.Next() {
		var p uint8
		var c uint64
		if rs.Scan(&p, &c) == nil {
			agg[protoName(p)] += c
			sum += c
		}
	}
	if sum == 0 {
		return nil
	}
	colors := map[string]string{"TCP": "#0077b6", "UDP": "#2a9d8f", "ICMP": "#e76f51", "OTH": "#9aa5b1"}
	var out []protoSlice
	for _, name := range []string{"TCP", "UDP", "ICMP", "OTH"} {
		if agg[name] > 0 {
			out = append(out, protoSlice{Name: name, Pct: round1(float64(agg[name]) / float64(sum) * 100), Color: colors[name]})
		}
	}
	return out
}

func (r *FlowReader) regionByDevice(ctx context.Context, ispID uint32, days int) [][]any {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT device_id, count() c FROM %s.flow_logs WHERE %s GROUP BY device_id ORDER BY c DESC LIMIT 7`, r.db, where)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var out [][]any
	for rs.Next() {
		var dev uint32
		var c uint64
		if rs.Scan(&dev, &c) == nil {
			out = append(out, []any{fmt.Sprintf("DEV-%d", dev), c})
		}
	}
	return out
}

func (r *FlowReader) topSubsByBytes(ctx context.Context, ispID uint32, days int) []topSub {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT src_ip, sum(bytes) b FROM %s.flow_logs WHERE %s GROUP BY src_ip ORDER BY b DESC LIMIT 5`, r.db, where)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var subs []struct {
		ip string
		b  uint64
	}
	var max uint64
	for rs.Next() {
		var ip net.IP
		var b uint64
		if rs.Scan(&ip, &b) == nil {
			subs = append(subs, struct {
				ip string
				b  uint64
			}{ip.String(), b})
			if b > max {
				max = b
			}
		}
	}
	var out []topSub
	for _, s := range subs {
		out = append(out, topSub{Label: s.ip, Value: humanBytes2(s.b), Pct: pctOf(s.b, max)})
	}
	return out
}

// ---- small format helpers ----
func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// ---- HTTP handlers ----

type meResp struct {
	Email      string `json:"email"`
	Role       string `json:"role"`
	ISPID      uint32 `json:"ispId"`
	IsDirector bool   `json:"isDirector"`
	CSRF       string `json:"csrf"`
}

func (s *Server) currentIdentity(r *http.Request) (Identity, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return Identity{}, false
	}
	id, ok := s.parseSession(c.Value)
	if !ok || !s.revalidateSessions {
		return id, ok
	}
	u, err := s.store.GetUser(r.Context(), id.UserID)
	if err != nil || u.Email != id.Email || u.ISPID != id.ISPID || u.Role != id.Role || !s.ispLoginAllowed(r.Context(), u) {
		return Identity{}, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.currentIdentity(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	writeJSON(w, http.StatusOK, meResp{Email: id.Email, Role: string(id.Role), ISPID: id.ISPID, IsDirector: id.isDirector(), CSRF: s.csrfToken(id)})
}

func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct{ Email, Password, TOTP string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)
	if clientIP == "" {
		clientIP = r.RemoteAddr
	}
	throttleKey := clientIP + ":" + email
	if s.loginThrottle != nil {
		if locked, remaining := s.loginThrottle.Check(throttleKey); locked {
			w.Header().Set("Retry-After", fmt.Sprint(int(remaining.Seconds())))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed login attempts"})
			return
		}
	}
	u, err := s.store.GetUserByLogin(r.Context(), email)
	hash := s.dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	if !VerifyPassword(hash, body.Password) || err != nil {
		if s.loginThrottle != nil {
			s.loginThrottle.RecordFailure(throttleKey)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if u.TOTPEnabled && !s.verifyUserTOTP(u, body.TOTP, time.Now()) {
		if s.loginThrottle != nil {
			s.loginThrottle.RecordFailure(throttleKey)
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid authenticator code", "totpRequired": "true"})
		return
	}
	if !s.ispLoginAllowed(r.Context(), u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this ISP account is disabled"})
		return
	}
	if s.loginThrottle != nil {
		s.loginThrottle.RecordSuccess(throttleKey)
	}
	id := Identity{UserID: u.ID, ISPID: u.ISPID, Role: u.Role, Email: u.Email, Exp: time.Now().Add(sessionTTL).Unix()}
	s.setSession(w, id)
	writeJSON(w, http.StatusOK, meResp{Email: id.Email, Role: string(id.Role), ISPID: id.ISPID, IsDirector: id.isDirector(), CSRF: s.csrfToken(id)})
}

func (s *Server) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	id, ok := s.currentIdentity(r)
	if ok && !s.validCSRF(id, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid csrf"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleConsoleData(w http.ResponseWriter, r *http.Request) {
	id, ok := s.currentIdentity(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	if s.flows == nil {
		writeJSON(w, http.StatusOK, consoleData{Empty: true})
		return
	}
	// Serve a recent snapshot if fresh: the dashboard aggregates over a large
	// window are expensive at high volume, so cache per tenant for a short TTL
	// (dashboards don't need per-second freshness) — repeated loads are instant
	// and the box is scanned at most once per TTL per tenant.
	// Serve-stale-while-revalidate: if we have ANY cached snapshot, return it
	// instantly; if it's past the TTL, kick off a background refresh. Only the
	// very first load per tenant blocks on the (expensive) compute.
	k := consoleKey{isp: id.ISPID, days: consoleDays(r.URL.Query().Get("days"), s.flowDays)}
	if d, fresh, exists := s.cachedConsole(k); exists {
		writeJSON(w, http.StatusOK, d)
		if !fresh {
			s.refreshConsoleAsync(k)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	data := s.flows.ConsoleData(ctx, k.isp, k.days)
	s.nameDevices(ctx, k.isp, data.Records)
	s.storeConsole(k, data)
	writeJSON(w, http.StatusOK, data)
}

const consoleCacheTTL = 45 * time.Second

type consoleCacheEntry struct {
	at         time.Time
	data       consoleData
	refreshing bool
}

// consoleKey scopes the dashboard cache. The window is part of the key because
// the console's time-range switcher asks for different ones, and a 24h snapshot
// must never be handed back to a request for 30d.
type consoleKey struct {
	isp  uint32
	days int
}

// consoleDays clamps the window a caller may request to the ranges the switcher
// offers, so an arbitrary ?days= cannot turn one request into a full-table scan.
func consoleDays(raw string, def int) int {
	switch raw {
	case "0":
		return 0
	case "1":
		return 1
	case "7":
		return 7
	case "30":
		return 30
	}
	return def
}

// cachedConsole returns the snapshot, whether it's within TTL (fresh), and
// whether any snapshot exists.
func (s *Server) cachedConsole(k consoleKey) (data consoleData, fresh, exists bool) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	e, ok := s.consoleCache[k]
	if !ok {
		return consoleData{}, false, false
	}
	return e.data, time.Since(e.at) < consoleCacheTTL, true
}

func (s *Server) storeConsole(k consoleKey, d consoleData) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if s.consoleCache == nil {
		s.consoleCache = map[consoleKey]consoleCacheEntry{}
	}
	s.consoleCache[k] = consoleCacheEntry{at: time.Now(), data: d}
}

// refreshConsoleAsync recomputes a tenant's dashboard in the background, at most
// one refresh in flight per tenant (so a burst of stale hits can't stampede).
func (s *Server) refreshConsoleAsync(k consoleKey) {
	if s.flows == nil {
		return
	}
	s.consoleMu.Lock()
	e := s.consoleCache[k]
	if e.refreshing {
		s.consoleMu.Unlock()
		return
	}
	e.refreshing = true
	s.consoleCache[k] = e
	s.consoleMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		data := s.flows.ConsoleData(ctx, k.isp, k.days)
		s.nameDevices(ctx, k.isp, data.Records)
		s.consoleMu.Lock()
		defer s.consoleMu.Unlock()
		// ConsoleData zero-fills on query error, so a transient ClickHouse hiccup
		// yields an Empty snapshot. Don't overwrite a previously-good one with it —
		// keep serving the last good data (just clear the refreshing flag) rather
		// than flashing a blank "no data" dashboard until the next refresh.
		if prev, ok := s.consoleCache[k]; ok && data.Empty && !prev.data.Empty {
			prev.refreshing = false
			s.consoleCache[k] = prev
			return
		}
		s.consoleCache[k] = consoleCacheEntry{at: time.Now(), data: data}
	}()
}

// nameDevices resolves the actual exporter within its tenant. Device IDs may
// intentionally be shared by two router uplinks; they are not registry identities.
func (s *Server) nameDevices(ctx context.Context, ispID uint32, rows []natRecord) {
	if len(rows) == 0 {
		return
	}
	devs, err := s.store.ListDevices(ctx, ispID)
	if err != nil {
		return
	}
	for i := range rows {
		if d, ok := resolveRecordDevice(devs, ispID, rows[i]); ok && d.Name != "" {
			rows[i].Sub = d.Name
		} else if rows[i].ExporterIP != "" {
			rows[i].Sub = rows[i].ExporterIP
		}
	}
}

// serveConsole serves the embedded single-page console.
func (s *Server) serveConsole(w http.ResponseWriter, _ *http.Request) {
	b, err := consoleFS.ReadFile("web/console/index.html")
	if err != nil {
		http.Error(w, "console not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}
