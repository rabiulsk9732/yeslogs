package director

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

type exportJob struct {
	ID         string    `json:"id"`
	Owner      string    `json:"owner"`
	ISPID      uint32    `json:"ispId"`
	Status     string    `json:"status"`
	Rows       int       `json:"rows"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
	path       string
}
type exportRequest struct {
	Filter SearchFilter `json:"filter"`
	CaseID int64        `json:"caseId"`
	Reason string       `json:"reason"`
	Schema string       `json:"schema"`
}

func exportID() string { var b [12]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (s *Server) handleExportCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	if !id.canExport() {
		writeJSON(w, 403, map[string]string{"error": "role cannot export"})
		return
	}
	if s.flows == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "flow store unavailable"})
		return
	}
	var req exportRequest
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	scope, e := id.scopeISP(req.Filter.ISPID)
	if e != nil || scope == 0 {
		writeJSON(w, 403, map[string]string{"error": "choose a permitted ISP"})
		return
	}
	req.Filter.ISPID = scope
	if e := validateSearchIPs(req.Filter); e != nil {
		writeJSON(w, 422, map[string]string{"error": e.Error()})
		return
	}
	if !req.Filter.HasSelector() {
		writeJSON(w, 422, map[string]string{"error": "specify an IP or device filter"})
		return
	}
	if req.CaseID > 0 {
		c, e := s.store.GetInvestigation(r.Context(), req.CaseID)
		if e != nil || c.ISPID != scope {
			writeJSON(w, 422, map[string]string{"error": "invalid case"})
			return
		}
		req.Reason = c.Reference
	}
	j := &exportJob{ID: exportID(), Owner: id.Email, ISPID: scope, Status: "queued", CreatedAt: time.Now().UTC()}
	s.exportMu.Lock()
	s.exportJobs[j.ID] = j
	s.exportMu.Unlock()
	queued := *j
	go s.runExport(j, req)
	writeJSON(w, http.StatusAccepted, queued)
}
func (s *Server) runExport(j *exportJob, req exportRequest) {
	set := func(status, err string) {
		s.exportMu.Lock()
		j.Status = status
		j.Error = err
		if status == "complete" || status == "failed" {
			j.FinishedAt = time.Now().UTC()
		}
		s.exportMu.Unlock()
	}
	set("running", "")
	f, e := os.CreateTemp("", "yeslogs-export-*.csv")
	if e != nil {
		set("failed", e.Error())
		return
	}
	path := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(path)
		}
	}()
	cw := csv.NewWriter(f)
	schema := strings.ToLower(req.Schema)
	meta := reportMeta{CaseRef: req.Reason, GeneratedBy: j.Owner, GeneratedAt: time.Now().In(istLoc).Format("2006-01-02 15:04:05 IST"), QueryIP: firstNonEmpty(req.Filter.PublicIP, req.Filter.PrivateIP, req.Filter.DestIP), QueryPort: req.Filter.PublicPort, QueryProto: req.Filter.Proto, From: tdisp(req.Filter.From), To: tdisp(req.Filter.To), Format: schema}
	cols, _ := reportColumns(meta)
	if e = cw.Write(cols); e != nil {
		set("failed", e.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	const page = 5000
	rowsWritten := 0
	for offset := 0; offset < 5000000; offset += page {
		rows, e := s.flows.Search(ctx, req.Filter, page, offset)
		if e != nil {
			set("failed", e.Error())
			return
		}
		s.nameDevices(ctx, j.ISPID, rows)
		s.enrichCRMRows(ctx, j.ISPID, rows)
		for _, row := range rows {
			if e = cw.Write(safeCells(rowCells(meta, row))); e != nil {
				set("failed", e.Error())
				return
			}
			rowsWritten++
		}
		cw.Flush()
		if e = cw.Error(); e != nil {
			set("failed", e.Error())
			return
		}
		if len(rows) < page {
			break
		}
	}
	if e = f.Sync(); e != nil {
		set("failed", e.Error())
		return
	}
	s.exportMu.Lock()
	j.path = path
	j.Rows = rowsWritten
	s.exportMu.Unlock()
	ok = true
	_, _ = s.store.LogQuery(context.Background(), store.QueryAudit{UserEmail: j.Owner, ISPID: j.ISPID, QueryIP: meta.QueryIP, QueryPort: meta.QueryPort, QueryProto: meta.QueryProto, FromTS: req.Filter.From, ToTS: req.Filter.To, ResultCount: rowsWritten, CaseRef: req.Reason + " [export:csv-async]"})
	set("complete", "")
}
func (s *Server) ownedExport(w http.ResponseWriter, r *http.Request) (Identity, exportJob, bool) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return id, exportJob{}, false
	}
	s.exportMu.Lock()
	ptr := s.exportJobs[r.PathValue("id")]
	var j exportJob
	if ptr != nil {
		j = *ptr
	}
	s.exportMu.Unlock()
	if ptr == nil || (!id.isDirector() && (j.Owner != id.Email || j.ISPID != id.ISPID)) {
		writeJSON(w, 404, map[string]string{"error": "export not found"})
		return id, exportJob{}, false
	}
	return id, j, true
}
func (s *Server) handleExportStatus(w http.ResponseWriter, r *http.Request) {
	_, j, ok := s.ownedExport(w, r)
	if ok {
		writeJSON(w, 200, j)
	}
}
func (s *Server) handleExportDownload(w http.ResponseWriter, r *http.Request) {
	id, j, ok := s.ownedExport(w, r)
	if !ok {
		return
	}
	if !id.canExport() {
		writeJSON(w, 403, map[string]string{"error": "role cannot export"})
		return
	}
	if j.Status != "complete" {
		writeJSON(w, 409, map[string]string{"error": "export is not complete"})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="yeslogs-export-%s.csv"`, j.ID))
	http.ServeFile(w, r, j.path)
}
