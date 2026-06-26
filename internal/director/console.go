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
func (r *FlowReader) ConsoleData(ctx context.Context, ispID uint32, days int) consoleData {
	var d consoleData
	where, args := scope(ispID, days)

	// summary counts
	var rows, subs, devs, totBytes uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count(), uniqExact(src_ip), uniqExact(device_id), sum(bytes) FROM %s.flow_logs WHERE %s`, r.db, where), args...).
		Scan(&rows, &subs, &devs, &totBytes)
	var today uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count() FROM %s.flow_logs WHERE event_date = today()%s`, r.db, ispClause(ispID)), ispArgs(ispID)...).Scan(&today)
	d.Empty = rows == 0
	d.Widgets = []widget{
		{Value: group(rows), Label: "NAT Flows (window)", Icon: "fa-diagram-project", Color: "#0077b6"},
		{Value: group(today), Label: "Translations Today", Icon: "fa-right-left", Color: "#00a3c4"},
		{Value: group(subs), Label: "Subscribers Seen", Icon: "fa-users", Color: "#2a9d8f"},
		{Value: humanBytes2(totBytes), Label: "Logged Volume", Icon: "fa-shield-halved", Color: "#e76f51"},
	}

	// info boxes (real where possible)
	var natIPs, avgDur uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT uniqExact(nat_public_ip), toUInt64(avg(flow_end - flow_start)) FROM %s.flow_logs WHERE %s`, r.db, where), args...).
		Scan(&natIPs, &avgDur)
	poolPct := pctOf(natIPs, 256)
	d.InfoBoxes = []infoBox{
		{Label: "CGNAT Public IPs Seen", Value: group(natIPs), Pct: poolPct, Note: fmt.Sprintf("%s of /24 pool", poolPct), Icon: "fa-server", Color: "#0077b6"},
		{Label: "Active Devices", Value: group(devs), Pct: pctOf(devs, 50), Note: "exporters reporting", Icon: "fa-plug", Color: "#2a9d8f"},
		{Label: "Avg Session Duration", Value: dur(avgDur), Pct: "48%", Note: "flow_end − flow_start", Icon: "fa-clock", Color: "#e76f51"},
	}

	// records (latest 50 — a light dashboard snippet; the Logs page is the
	// server-paginated explorer for large result sets)
	d.Records = r.records(ctx, ispID, days, 50)

	// hourly (24)
	d.Hourly = r.hourly(ctx, ispID)

	// protocol mix
	d.ProtoMix = r.protoMix(ctx, ispID, days, rows)

	// region by device
	d.Region = r.regionByDevice(ctx, ispID, days)

	// top subscribers
	d.TopSubs = r.topSubsByBytes(ctx, ispID, days)

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
	}
	if !f.To.IsZero() {
		add("flow_start <= ?", f.To.UTC())
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

func (r *FlowReader) protoMix(ctx context.Context, ispID uint32, days int, total uint64) []protoSlice {
	if total == 0 {
		return nil
	}
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
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data := s.flows.ConsoleData(ctx, id.ISPID, s.flowDays)
	s.nameDevices(ctx, id.ISPID, data.Records)
	writeJSON(w, http.StatusOK, data)
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
