package director

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
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

// daysOlderThan returns the distinct hot-storage days (YYYY-MM-DD) older than the
// hot window — candidates for cold-archival to S3.
func (r *FlowReader) daysOlderThan(ctx context.Context, afterDays int) []string {
	if afterDays < 0 {
		afterDays = 0
	}
	q := fmt.Sprintf(`SELECT DISTINCT toString(event_date) FROM %s.flow_logs WHERE event_date < today() - %d ORDER BY event_date`, r.db, afterDays)
	rows, err := r.conn.Query(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if rows.Scan(&d) == nil {
			out = append(out, d)
		}
	}
	return out
}

// DropDay drops one day's partition from hot storage (instant — the table is
// PARTITION BY event_date). Only called after the day is safely in S3.
func (r *FlowReader) DropDay(ctx context.Context, day string) error {
	if _, err := time.Parse("2006-01-02", day); err != nil { // day is code-controlled; validate anyway
		return fmt.Errorf("bad partition day %q", day)
	}
	return r.conn.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s.flow_logs DROP PARTITION '%s'`, r.db, day))
}

// ArchiveSweep moves every hot day older than the configured hot window to S3
// (all ISPs, one object per ISP), then drops it from hot storage. Idempotent: a
// day already recorded as archived is skipped, and a day whose upload fails is
// left in hot storage to retry on the next sweep. Director/central only.
func (s *Server) ArchiveSweep(ctx context.Context) (days int, rows, bytes int64, err error) {
	set := s.CurrentSettings().S3
	arch, _, format := s.archInfo()
	if arch == nil || !set.AutoArchive || s.flows == nil {
		return 0, 0, 0, nil
	}
	after := set.ArchiveAfterDays
	if after < 1 {
		after = 7
	}
	isps, e := s.store.ListISPs(ctx)
	if e != nil {
		return 0, 0, 0, e
	}
	for _, day := range s.flows.daysOlderThan(ctx, after) {
		if done, _ := s.store.IsDayArchived(ctx, day); done {
			continue
		}
		pd, perr := time.ParseInLocation("2006-01-02", day, istLoc)
		if perr != nil {
			continue
		}
		var dRows, dBytes int64
		var objs int
		ok := true
		for _, isp := range isps {
			res, ee := arch.ExportDay(ctx, isp.ID, pd, format)
			if ee != nil {
				ok = false
				s.log.Error("auto-archive export", "day", day, "isp", isp.ID, "error", ee)
				break
			}
			dRows += res.Rows
			dBytes += res.Bytes
			if res.Key != "" {
				objs++
			}
		}
		if !ok {
			continue // leave hot data for the next sweep
		}
		if derr := s.flows.DropDay(ctx, day); derr != nil {
			s.log.Error("auto-archive drop partition", "day", day, "error", derr)
			continue
		}
		_ = s.store.MarkDayArchived(ctx, store.ArchivedDay{Day: day, Objects: objs, Rows: dRows, Bytes: dBytes})
		s.log.Info("auto-archived day to S3", "day", day, "rows", dRows, "bytes", dBytes, "objects", objs)
		days++
		rows += dRows
		bytes += dBytes
	}
	return days, rows, bytes, nil
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
	set := s.CurrentSettings().S3
	archInfo := map[string]any{
		"enabled": arch != nil, "bucket": bucket, "format": format,
		"canRun": arch != nil && id.isDirector(),
		"auto":   set.AutoArchive, "afterDays": set.ArchiveAfterDays,
	}
	if id.isDirector() {
		if ad, e := s.store.ListArchivedDays(ctx, 60); e == nil {
			archInfo["archivedDays"] = ad
		}
	}
	resp := map[string]any{
		"available":     true,
		"retentionDays": retDays,
		"storage":       map[string]any{"rows": rows, "bytes": bytes, "human": humanBytes(bytes)},
		"window":        map[string]string{"from": mn, "to": mx},
		"perDay":        s.flows.perDay(ctx, id.ISPID, 30),
		"archive":       archInfo,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleArchiveSweep runs the auto-archival sweep on demand (director only).
func (s *Server) handleArchiveSweep(w http.ResponseWriter, r *http.Request) {
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
	if arch, _, _ := s.archInfo(); arch == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "S3 archive not configured"})
		return
	}
	if !s.CurrentSettings().S3.AutoArchive {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "auto-archive is disabled in Settings → S3"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	days, rows, bytes, err := s.ArchiveSweep(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "rows": rows, "bytes": bytes, "human": humanBytes(uint64(bytes))})
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
