package director

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

const searchLimit = 5000

// parseTime accepts datetime-local ("2006-01-02T15:04"), "2006-01-02 15:04[:05]",
// or RFC3339. Empty string → zero time (unbounded).
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, istLoc); err == nil { // form input is IST
			return t
		}
	}
	return time.Time{}
}

func tdisp(t time.Time) string {
	if t.IsZero() {
		return "any"
	}
	return t.Format("2006-01-02 15:04")
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// handleSearch runs a flow-log search (public/private/destination IP + device +
// time) and records it in the query audit.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	if s.flows == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "flow store unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		PublicIP, PrivateIP, DestIP string
		PublicPort                  int
		Proto, From, To, Reason     string
		DeviceID                    uint32
		ISPID                       uint32
		Limit, Offset               int
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	limit := body.Limit
	if limit < 1 || limit > 200 { // page size: 1..200 rows, default 50
		limit = 50
	}
	offset := body.Offset
	if offset < 0 {
		offset = 0
	}
	scope, err := id.scopeISP(body.ISPID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	f := SearchFilter{
		ISPID: scope, PublicIP: strings.TrimSpace(body.PublicIP), PrivateIP: strings.TrimSpace(body.PrivateIP),
		DestIP: strings.TrimSpace(body.DestIP), PublicPort: body.PublicPort, Proto: body.Proto,
		DeviceID: body.DeviceID, From: parseTime(body.From), To: parseTime(body.To),
	}
	if !f.HasSelector() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "specify at least a Public IP, Private IP, Destination IP, or Device"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second) // cold (S3) search can be slower
	defer cancel()
	rows, total, cold, err := s.searchAll(ctx, f, limit, offset)
	if err != nil {
		s.log.Error("flow search", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}
	s.nameDevices(ctx, scope, rows)
	if offset == 0 { // audit the query once (on the first page), not every page-flip
		_, _ = s.store.LogQuery(ctx, store.QueryAudit{
			UserEmail: id.Email, ISPID: scope, QueryIP: firstNonEmpty(f.PublicIP, f.PrivateIP, f.DestIP),
			QueryPort: f.PublicPort, QueryProto: f.Proto, FromTS: f.From, ToTS: f.To,
			ResultCount: int(total), CaseRef: strings.TrimSpace(body.Reason),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": rows, "count": len(rows), "total": total, "offset": offset, "limit": limit,
		"capped": total >= countCap, "cold": cold,
	})
}

// handleReport re-runs a search and streams it as CSV/PDF/XLSX (read-only GET),
// recording the export in the audit.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.currentIdentity(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	// CSRF: this is a state-recording GET (writes an audit row) reached by a direct
	// link, so it carries the session-bound token as a query param instead of a header.
	if !s.validCSRF(id, r.URL.Query().Get("csrf")) {
		http.Error(w, "invalid csrf", http.StatusForbidden)
		return
	}
	if s.flows == nil {
		http.Error(w, "flow store unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	scope, err := id.scopeISP(parseUint32(q.Get("isp")))
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	f := SearchFilter{
		ISPID: scope, PublicIP: strings.TrimSpace(q.Get("ip")), PrivateIP: strings.TrimSpace(q.Get("priv")),
		DestIP: strings.TrimSpace(q.Get("dst")), PublicPort: int(parseUint32(q.Get("port"))), Proto: q.Get("proto"),
		DeviceID: parseUint32(q.Get("device")), From: parseTime(q.Get("from")), To: parseTime(q.Get("to")),
	}
	if !f.HasSelector() {
		http.Error(w, "specify at least one IP or device filter", http.StatusBadRequest)
		return
	}
	format := strings.ToLower(q.Get("format"))
	if format != "pdf" && format != "xlsx" {
		format = "csv"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second) // reports may span cold (S3) days
	defer cancel()
	rows, _, _, err := s.searchAll(ctx, f, searchLimit, 0)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	s.nameDevices(ctx, scope, rows)
	_, _ = s.store.LogQuery(ctx, store.QueryAudit{
		UserEmail: id.Email, ISPID: scope, QueryIP: firstNonEmpty(f.PublicIP, f.PrivateIP, f.DestIP),
		QueryPort: f.PublicPort, QueryProto: f.Proto, FromTS: f.From, ToTS: f.To,
		ResultCount: len(rows), CaseRef: strings.TrimSpace(q.Get("reason")) + " [export:" + format + "]",
	})
	meta := reportMeta{
		CaseRef: q.Get("reason"), GeneratedBy: id.Email,
		GeneratedAt: time.Now().In(istLoc).Format("2006-01-02 15:04:05 IST"),
		QueryIP:     firstNonEmpty(f.PublicIP, f.PrivateIP, f.DestIP), QueryPort: f.PublicPort, QueryProto: f.Proto,
		From: tdisp(f.From), To: tdisp(f.To), Count: len(rows), Truncated: len(rows) == searchLimit,
	}
	// e.g. iplog-report-45.115.107.52-222000_02.03.2026_IST_0530.pdf
	now := time.Now().In(istLoc)
	base := fmt.Sprintf("iplog-report-%s-%s_%s_IST_0530",
		firstNonEmpty(meta.QueryIP, "all"), now.Format("150405"), now.Format("02.01.2006"))

	var werr error
	switch format {
	case "pdf":
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.pdf"`)
		werr = writePDF(w, meta, rows)
	case "xlsx":
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.xlsx"`)
		werr = writeXLSX(w, meta, rows)
	default:
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.csv"`)
		werr = writeCSV(w, meta, rows)
	}
	if werr != nil {
		s.log.Error("report export failed", "format", format, "error", werr)
	}
}

// handleAudit returns recent query/export audit records (tenant-scoped).
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	q, err := s.store.ListQueries(r.Context(), tenantScope(id), 100)
	if err != nil {
		s.log.Error("list audit", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	for i := range q { // present audit times in IST
		q[i].CreatedAt = q[i].CreatedAt.In(istLoc)
		if !q[i].FromTS.IsZero() {
			q[i].FromTS = q[i].FromTS.In(istLoc)
		}
		if !q[i].ToTS.IsZero() {
			q[i].ToTS = q[i].ToTS.In(istLoc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"queries": nz(q)})
}
