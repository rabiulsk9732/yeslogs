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
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
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

// handleSearch runs a lawful IPDR lookup (public endpoint + window → subscriber)
// and records it in the query audit.
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
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		IP, Proto, From, To, CaseRef string
		Port                         int
		ISPID                        uint32
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	ip := strings.TrimSpace(body.IP)
	if ip == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public IP is required"})
		return
	}
	scope, err := id.scopeISP(body.ISPID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	from, to := parseTime(body.From), parseTime(body.To)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	rows, err := s.flows.SearchByPublic(ctx, scope, ip, body.Port, body.Proto, from, to, searchLimit)
	if err != nil {
		s.log.Error("ipdr search", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "search failed"})
		return
	}
	// Lawful-intercept audit: record every lookup.
	_, _ = s.store.LogQuery(ctx, store.QueryAudit{
		UserEmail: id.Email, ISPID: scope, QueryIP: ip, QueryPort: body.Port,
		QueryProto: body.Proto, FromTS: from, ToTS: to, ResultCount: len(rows), CaseRef: strings.TrimSpace(body.CaseRef),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"records": rows, "count": len(rows), "truncated": len(rows) == searchLimit,
	})
}

// handleReport re-runs the lookup and streams it as CSV/PDF/XLSX (read-only GET).
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, ok := s.currentIdentity(r)
	if !ok {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	if s.flows == nil {
		http.Error(w, "flow store unavailable", http.StatusServiceUnavailable)
		return
	}
	q := r.URL.Query()
	ip := strings.TrimSpace(q.Get("ip"))
	if ip == "" {
		http.Error(w, "public IP required", http.StatusBadRequest)
		return
	}
	scope, err := id.scopeISP(parseUint32(q.Get("isp")))
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	port := int(parseUint32(q.Get("port")))
	proto := q.Get("proto")
	from, to := parseTime(q.Get("from")), parseTime(q.Get("to"))
	format := strings.ToLower(q.Get("format"))

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := s.flows.SearchByPublic(ctx, scope, ip, port, proto, from, to, searchLimit)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	if format != "pdf" && format != "xlsx" {
		format = "csv"
	}
	// Lawful audit: an export is itself an auditable data extraction.
	_, _ = s.store.LogQuery(ctx, store.QueryAudit{
		UserEmail: id.Email, ISPID: scope, QueryIP: ip, QueryPort: port, QueryProto: proto,
		FromTS: from, ToTS: to, ResultCount: len(rows),
		CaseRef: strings.TrimSpace(q.Get("case")) + " [export:" + format + "]",
	})
	meta := reportMeta{
		CaseRef: q.Get("case"), GeneratedBy: id.Email,
		GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05 UTC"),
		QueryIP:     ip, QueryPort: port, QueryProto: proto,
		From: tdisp(from), To: tdisp(to), Count: len(rows), Truncated: len(rows) == searchLimit,
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	base := fmt.Sprintf("ipdr-%s-%s", strings.ReplaceAll(ip, ".", "_"), stamp)

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
	default: // csv
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.csv"`)
		werr = writeCSV(w, meta, rows)
	}
	if werr != nil {
		s.log.Error("report export failed", "format", format, "error", werr)
	}
}

// handleAudit returns recent lawful-query audit records (tenant-scoped).
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
	writeJSON(w, http.StatusOK, map[string]any{"queries": q})
}
