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

// window returns the first and last day held in hot storage. event_date is the
// partition key, so for the fleet-wide view the answer already sits in
// system.parts and needs no data read at all — 0.10s against 1.08s for the
// scan. A tenant view still has to look at rows: parts are not split by isp_id.
func (r *FlowReader) window(ctx context.Context, ispID uint32) (string, string) {
	if ispID == 0 {
		var mn, mx string
		if err := r.conn.QueryRow(ctx,
			`SELECT min(partition), max(partition) FROM system.parts WHERE database = ? AND table = 'flow_logs' AND active`,
			r.db).Scan(&mn, &mx); err == nil && mn != "" {
			return mn, mx
		}
		// fall through to the scan if the metadata read failed
	}
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

// perDay returns record counts per day, newest first. limit <= 0 returns the
// whole data window (naturally bounded by the retention TTL) — the console
// table paginates/filters client-side, so it wants every day, not a fixed 30.
func (r *FlowReader) perDay(ctx context.Context, ispID uint32, limit int) []dayCount {
	where, args := "1", []any(nil)
	if ispID != 0 {
		where, args = "isp_id = ?", []any{ispID}
	}
	limitClause := ""
	if limit > 0 {
		limitClause = fmt.Sprintf(" LIMIT %d", limit)
	}
	// Same trick as window(): one row per day is exactly what the parts table
	// already counts, so the fleet-wide list costs 0.08s instead of a 2.0s
	// GROUP BY over every row. Verified to return identical counts.
	if ispID == 0 {
		q := fmt.Sprintf(`SELECT partition, toUInt64(sum(rows)) FROM system.parts
			WHERE database = '%s' AND table = 'flow_logs' AND active
			GROUP BY partition ORDER BY partition DESC%s`, r.db, limitClause)
		if rows, err := r.conn.Query(ctx, q); err == nil {
			defer rows.Close()
			var out []dayCount
			for rows.Next() {
				var d dayCount
				if err := rows.Scan(&d.Date, &d.Count); err != nil {
					out = nil
					break
				}
				out = append(out, d)
			}
			if out != nil {
				return out
			}
		}
		// fall through to the scan if the metadata read failed
	}
	q := fmt.Sprintf(`SELECT toString(event_date), count() FROM %s.flow_logs WHERE %s GROUP BY event_date ORDER BY event_date DESC%s`, r.db, where, limitClause)
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
// rowsOnDay counts every row in one day's partition, regardless of tenant. Used
// to prove an export covered the whole partition before it is dropped.
func (r *FlowReader) rowsOnDay(ctx context.Context, day string) (int64, error) {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return 0, fmt.Errorf("bad partition day %q", day)
	}
	var n uint64
	err := r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT count() FROM %s.flow_logs WHERE event_date = ?`, r.db), day).Scan(&n)
	return int64(n), err
}

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
	s.archiveSweepMu.Lock()
	defer s.archiveSweepMu.Unlock()
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
			// Already in S3 but still present in hot → a previous drop must have failed.
			// Reconcile by dropping now so hot+cold never both serve the same day.
			if derr := s.flows.DropDay(ctx, day); derr != nil {
				s.log.Error("auto-archive: re-drop of already-archived day failed", "day", day, "error", derr)
			}
			continue
		}
		// Parse as UTC midnight: ExportDay/ArchiveRel key off day.UTC(), so an
		// IST-midnight time would shift the queried date back a day (data loss).
		pd, perr := time.Parse("2006-01-02", day)
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
		// Safety: daysOlderThan only returns days that HAVE rows, so exporting 0
		// rows means a mismatch (or rows belong to a deleted ISP) — never drop then.
		if dRows == 0 {
			s.log.Error("auto-archive: candidate day exported 0 rows; NOT dropping from hot", "day", day)
			continue
		}
		// The export loop covers only ISPs registered in the control plane, but
		// DropDay drops the entire partition. Any row belonging to no registered
		// ISP — isp_id 0 from observe mode or from a default-tenant config, or an
		// ISP since deleted — would be destroyed having never been written to S3.
		// dRows > 0 does not rule that out: the registered tenants' rows alone
		// make it positive. Compare against what the partition actually holds and
		// refuse to drop unless every row was exported.
		hotRows, herr := s.flows.rowsOnDay(ctx, day)
		if herr != nil {
			s.log.Error("auto-archive: could not count hot rows; NOT dropping from hot", "day", day, "error", herr)
			continue
		}
		if hotRows != dRows {
			s.log.Error("auto-archive: partition holds rows that were not exported; NOT dropping from hot",
				"day", day, "hot_rows", hotRows, "exported_rows", dRows, "unexported", hotRows-dRows,
				"hint", "rows belong to no registered ISP (isp_id 0 from observe mode, or a deleted ISP); register the tenant or clear those rows deliberately")
			continue
		}
		// Record the archived marker BEFORE dropping from hot. The day is durably in
		// S3 now; if the marker write fails we must NOT drop it (cold search finds days
		// only via archived_days), otherwise it would be in S3 but invisible forever.
		// Leaving it in hot means the next sweep retries.
		if merr := s.store.MarkDayArchived(ctx, store.ArchivedDay{Day: day, Objects: objs, Rows: dRows, Bytes: dBytes}); merr != nil {
			s.log.Error("auto-archive: marker write failed; NOT dropping from hot (will retry)", "day", day, "error", merr)
			continue
		}
		if derr := s.flows.DropDay(ctx, day); derr != nil {
			// Marked archived but couldn't drop — harmless: day stays in both hot+S3;
			// next sweep sees it's already archived (IsDayArchived) and skips re-export.
			s.log.Error("auto-archive: marked but drop partition failed (still searchable in hot)", "day", day, "error", derr)
			continue
		}
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
		"perDay":        s.flows.perDay(ctx, id.ISPID, 0), // full window; table filters/sorts client-side
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
	// Record the archived marker. Cold search discovers days ONLY via
	// archived_days, so without this the objects sit in S3 unreachable: the day
	// still answers from hot until the 180-day TTL removes it, and from that
	// moment the records physically exist and can never be returned. Marking it
	// is safe while the day is also hot — archivedDaysInRange skips any day still
	// present in hot storage, so the two copies cannot both be read.
	//
	// Only on a clean export. A partial one would claim S3 holds the whole day.
	if failed == 0 && totalRows > 0 {
		if merr := s.store.MarkDayArchived(ctx, store.ArchivedDay{
			Day: day.Format("2006-01-02"), Objects: len(keys), Rows: totalRows, Bytes: totalBytes,
		}); merr != nil {
			s.log.Error("archive: export succeeded but marker write failed; the objects are in S3 but cold search cannot find them",
				"day", day.Format("2006-01-02"), "error", merr)
			resp := map[string]any{"rows": totalRows, "bytes": totalBytes, "objects": keys, "bucket": bucket,
				"isps": len(isps), "failed": failed,
				"error": "export succeeded but the archived-day marker could not be written; these objects will not be searchable — retry before the day leaves hot storage"}
			writeJSON(w, http.StatusInternalServerError, resp)
			return
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
