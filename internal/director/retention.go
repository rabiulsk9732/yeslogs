package director

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// ---- flow-store retention/storage queries ----

type dayCount struct {
	Date  string `json:"date"`
	Count uint64 `json:"count"`
}

// storageStats returns rows + bytes. A director (ispID 0) gets the global
// on-disk footprint from system.parts; a tenant gets ONLY its own scoped figures
// (never the cross-tenant total).
func (r *FlowReader) storageStats(ctx context.Context, ispID uint32) (rows, bytes uint64) {
	if ispID == 0 {
		_ = r.conn.QueryRow(ctx,
			`SELECT toUInt64(sum(rows)), toUInt64(sum(bytes_on_disk)) FROM system.parts WHERE database = ? AND table = 'flow_logs' AND active`,
			r.db).Scan(&rows, &bytes)
		return
	}
	_ = r.conn.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(), toUInt64(sum(bytes)) FROM %s.flow_logs WHERE isp_id = ?`, r.db),
		ispID).Scan(&rows, &bytes)
	return
}

func (r *FlowReader) window(ctx context.Context, ispID uint32) (string, string) {
	where, args := "1", []any(nil)
	if ispID != 0 {
		where, args = "isp_id = ?", []any{ispID}
	}
	var mn, mx time.Time
	if err := r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT min(event_date), max(event_date) FROM %s.flow_logs WHERE %s`, r.db, where), args...).Scan(&mn, &mx); err != nil {
		return "—", "—"
	}
	if mn.IsZero() {
		return "—", "—"
	}
	return mn.Format("2006-01-02"), mx.Format("2006-01-02")
}

func (r *FlowReader) perDay(ctx context.Context, ispID uint32, days int) []dayCount {
	where, args := "1", []any(nil)
	if ispID != 0 {
		where, args = "isp_id = ?", []any{ispID}
	}
	q := fmt.Sprintf(`SELECT toString(event_date), count() FROM %s.flow_logs WHERE %s GROUP BY event_date ORDER BY event_date DESC LIMIT %d`, r.db, where, days)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []dayCount
	for rows.Next() {
		var d dayCount
		if rows.Scan(&d.Date, &d.Count) == nil {
			out = append(out, d)
		}
	}
	return out
}

// SetTTLDays applies the retention window to the ClickHouse table TTL.
func (r *FlowReader) SetTTLDays(ctx context.Context, days int) error {
	if days < 1 {
		days = 1
	}
	return r.conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s.flow_logs MODIFY TTL event_date + INTERVAL %d DAY`, r.db, days))
}

// ---- handlers ----

func (s *Server) handleRetention(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if s.flows == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rows, bytes := s.flows.storageStats(ctx, id.ISPID)
	mn, mx := s.flows.window(ctx, id.ISPID)
	retDays := s.retDays()
	if retDays == 0 {
		retDays = 180
	}
	arch, bucket, format := s.archInfo()
	resp := map[string]any{
		"available":     true,
		"retentionDays": retDays,
		"storage":       map[string]any{"rows": rows, "bytes": bytes, "human": humanBytes(bytes)},
		"window":        map[string]string{"from": mn, "to": mx},
		"perDay":        s.flows.perDay(ctx, id.ISPID, 30),
		"archive":       map[string]any{"enabled": arch != nil, "bucket": bucket, "format": format, "canRun": arch != nil && id.isDirector()},
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArchive runs an S3 cold-archive export for a given day across all ISPs
// (director only).
func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	arch, bucket, format := s.archInfo()
	if arch == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "S3 archive not configured"})
		return
	}
	day, err := time.Parse("2006-01-02", r.PathValue("date"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid date (want YYYY-MM-DD)"})
		return
	}
	isps, err := s.store.ListISPs(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	var totalRows, totalBytes int64
	var keys []string
	var failed int
	var firstErr string
	for _, isp := range isps {
		res, err := arch.ExportDay(ctx, isp.ID, day, format)
		if err != nil {
			failed++
			if firstErr == "" {
				firstErr = err.Error()
			}
			s.log.Error("archive export", "isp", isp.ID, "day", day, "error", err)
			continue
		}
		totalRows += res.Rows
		totalBytes += res.Bytes
		if res.Key != "" {
			keys = append(keys, res.Key)
		}
	}
	resp := map[string]any{"rows": totalRows, "bytes": totalBytes, "objects": keys, "bucket": bucket, "isps": len(isps), "failed": failed}
	status := http.StatusOK
	if failed > 0 {
		resp["error"] = firstErr
		if len(keys) == 0 {
			status = http.StatusInternalServerError
		}
	}
	writeJSON(w, status, resp)
}
