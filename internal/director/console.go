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
	return http.FileServerFS(sub)
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
	Date     string `json:"date"`
	Clock    string `json:"clock"`
	Time     string `json:"time"`
	Sub      string `json:"sub"`
	DevID    uint32 `json:"devId"`
	PrivIP   string `json:"privIp"`
	PrivPort int    `json:"privPort"`
	PubIP    string `json:"pubIp"`
	PubPort  int    `json:"pubPort"`
	Proto    string `json:"proto"`
	Dest     string `json:"dest"`
	DstIP    string `json:"dstIp"`
	DstPort  int    `json:"dstPort"`
	Action   string `json:"action"`
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
	var rows, subs, devs, totBytes, natIPs, avgDur, today uint64
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() {
		_ = r.conn.QueryRow(ctx, fmt.Sprintf(
			`SELECT count(), uniq(src_ip), uniq(device_id), sum(bytes), uniq(nat_public_ip), toUInt64(avg(flow_end - flow_start))
			 FROM %s.flow_logs WHERE %s`, r.db, where), args...).
			Scan(&rows, &subs, &devs, &totBytes, &natIPs, &avgDur)
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
		{Label: "Avg Session Duration", Value: dur(avgDur), Pct: "48%", Note: "flow_end − flow_start", Icon: "fa-clock", Color: "#e76f51"},
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
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT flow_start, device_id, src_ip, src_port,
		if(nat_public_ip = toIPv4('0.0.0.0'), dst_ip, nat_public_ip) AS pubip,
		if(nat_public_port = 0, dst_port, nat_public_port) AS pubport,
		dst_ip, protocol, flow_type
		FROM %s.flow_logs WHERE %s ORDER BY flow_start DESC LIMIT %d`, r.db, where, limit)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	out := make([]natRecord, 0, limit)
	for rs.Next() {
		var ts time.Time
		var dev uint32
		var sp, pp uint16
		var sip, pip, dip net.IP
		var proto uint8
		var ft string
		if err := rs.Scan(&ts, &dev, &sip, &sp, &pip, &pp, &dip, &proto, &ft); err != nil {
			return out
		}
		out = append(out, natRecord{
			Date: ts.In(istLoc).Format("2006-01-02"), Clock: ts.In(istLoc).Format("15:04:05"), Time: ts.In(istLoc).Format("2006-01-02 15:04:05"),
			Sub: fmt.Sprintf("DEV-%d", dev), DevID: dev, PrivIP: sip.String(), PrivPort: int(sp),
			PubIP: pip.String(), PubPort: int(pp), Proto: protoName(proto), Dest: dip.String(),
			Action: strings.ToUpper(ft),
		})
	}
	return out
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
	Proto                       string
	DeviceID                    uint32
	From, To                    time.Time
}

// HasSelector reports whether the filter narrows the scan (an IP or device).
func (f SearchFilter) HasSelector() bool {
	return f.PublicIP != "" || f.PrivateIP != "" || f.DestIP != "" || f.DeviceID != 0
}

// hotWhere builds the WHERE clause + bound args for the hot flow_logs table.
func hotWhere(f SearchFilter) (string, []any, bool) {
	var conds []string
	var args []any
	add := func(c string, a any) { conds = append(conds, c); args = append(args, a) }
	if f.PublicIP != "" {
		add("nat_public_ip = toIPv4(?)", f.PublicIP)
	}
	if f.PrivateIP != "" {
		add("src_ip = toIPv4(?)", f.PrivateIP)
	}
	if f.DestIP != "" {
		add("dst_ip = toIPv4(?)", f.DestIP)
	}
	if f.PublicPort > 0 {
		add("nat_public_port = ?", uint16(f.PublicPort))
	}
	if pn := protoNum(f.Proto); pn > 0 {
		add("protocol = ?", pn)
	}
	if f.DeviceID > 0 {
		add("device_id = ?", f.DeviceID)
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

// SearchCount returns the number of hot rows matching f, capped at countCap
// (a returned value == countCap means "countCap or more").
func (r *FlowReader) SearchCount(ctx context.Context, f SearchFilter) uint64 {
	where, args, ok := hotWhere(f)
	if !ok {
		return 0
	}
	q := fmt.Sprintf(`SELECT count() FROM (SELECT 1 FROM %s.flow_logs WHERE %s LIMIT %d)`, r.db, where, countCap)
	var n uint64
	_ = r.conn.QueryRow(ctx, q, args...).Scan(&n)
	return n
}

// Search returns one page of flow-log records matching the filter (tenant-scoped
// by ISPID), ordered newest-first, using LIMIT/OFFSET server-side pagination so
// the client only ever holds a single page.
func (r *FlowReader) Search(ctx context.Context, f SearchFilter, limit, offset int) ([]natRecord, error) {
	where, args, ok := hotWhere(f)
	if !ok {
		return nil, fmt.Errorf("no filter")
	}
	q := fmt.Sprintf(`SELECT flow_start, device_id, src_ip, src_port, nat_public_ip, nat_public_port, dst_ip, dst_port, protocol, flow_type
		FROM %s.flow_logs WHERE %s ORDER BY flow_start DESC LIMIT %d OFFSET %d`, r.db, where, limit, offset)
	rs, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := make([]natRecord, 0, 128)
	for rs.Next() {
		var ts time.Time
		var dev uint32
		var sp, pp, dp uint16
		var sip, pip, dip net.IP
		var pr uint8
		var ft string
		if err := rs.Scan(&ts, &dev, &sip, &sp, &pip, &pp, &dip, &dp, &pr, &ft); err != nil {
			return out, err
		}
		out = append(out, natRecord{
			Date: ts.In(istLoc).Format("2006-01-02"), Clock: ts.In(istLoc).Format("15:04:05"), Time: ts.In(istLoc).Format("2006-01-02 15:04:05"),
			Sub: fmt.Sprintf("DEV-%d", dev), DevID: dev, PrivIP: sip.String(), PrivPort: int(sp),
			PubIP: pip.String(), PubPort: int(pp), Proto: protoName(pr),
			DstIP: dip.String(), DstPort: int(dp),
			Dest: fmt.Sprintf("%s:%d", dip.String(), dp), Action: strings.ToUpper(ft),
		})
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
func dur(sec uint64) string {
	return fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
}
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
	return s.parseSession(c.Value)
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
	var body struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	u, err := s.store.GetUserByEmail(r.Context(), email)
	hash := s.dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	if !VerifyPassword(hash, body.Password) || err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	if !s.ispLoginAllowed(r.Context(), u) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "this ISP account is disabled"})
		return
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
	if d, fresh, exists := s.cachedConsole(id.ISPID); exists {
		writeJSON(w, http.StatusOK, d)
		if !fresh {
			s.refreshConsoleAsync(id.ISPID)
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	data := s.flows.ConsoleData(ctx, id.ISPID, s.flowDays)
	s.nameDevices(ctx, id.ISPID, data.Records)
	s.storeConsole(id.ISPID, data)
	writeJSON(w, http.StatusOK, data)
}

const consoleCacheTTL = 45 * time.Second

type consoleCacheEntry struct {
	at         time.Time
	data       consoleData
	refreshing bool
}

// cachedConsole returns the snapshot, whether it's within TTL (fresh), and
// whether any snapshot exists.
func (s *Server) cachedConsole(ispID uint32) (data consoleData, fresh, exists bool) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	e, ok := s.consoleCache[ispID]
	if !ok {
		return consoleData{}, false, false
	}
	return e.data, time.Since(e.at) < consoleCacheTTL, true
}

func (s *Server) storeConsole(ispID uint32, d consoleData) {
	s.consoleMu.Lock()
	defer s.consoleMu.Unlock()
	if s.consoleCache == nil {
		s.consoleCache = map[uint32]consoleCacheEntry{}
	}
	s.consoleCache[ispID] = consoleCacheEntry{at: time.Now(), data: d}
}

// refreshConsoleAsync recomputes a tenant's dashboard in the background, at most
// one refresh in flight per tenant (so a burst of stale hits can't stampede).
func (s *Server) refreshConsoleAsync(ispID uint32) {
	if s.flows == nil {
		return
	}
	s.consoleMu.Lock()
	e := s.consoleCache[ispID]
	if e.refreshing {
		s.consoleMu.Unlock()
		return
	}
	e.refreshing = true
	s.consoleCache[ispID] = e
	s.consoleMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		data := s.flows.ConsoleData(ctx, ispID, s.flowDays)
		s.nameDevices(ctx, ispID, data.Records)
		s.consoleMu.Lock()
		s.consoleCache[ispID] = consoleCacheEntry{at: time.Now(), data: data}
		s.consoleMu.Unlock()
	}()
}

// nameDevices resolves each row's numeric device_id to its friendly device name
// (so flow tables show "wadhai-edge-01" instead of "DEV-1"). Falls back to the
// DEV-<id> label when a device is not found.
func (s *Server) nameDevices(ctx context.Context, ispID uint32, rows []natRecord) {
	if len(rows) == 0 {
		return
	}
	devs, err := s.store.ListDevices(ctx, ispID)
	if err != nil {
		return
	}
	m := make(map[uint32]string, len(devs))
	for _, d := range devs {
		if d.Name != "" {
			m[d.DeviceID] = d.Name
		}
	}
	for i := range rows {
		if n, ok := m[rows[i].DevID]; ok {
			rows[i].Sub = n
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
