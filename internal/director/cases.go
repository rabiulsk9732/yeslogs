package director

import (
	"encoding/json"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"net/http"
	"strconv"
	"strings"
)

func (s *Server) handleCases(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	cases, e := s.store.ListInvestigations(r.Context(), tenantScope(id))
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	out := map[string]any{"cases": nz(cases), "canManage": id.canManage()}
	if id.isDirector() {
		isps, _ := s.store.ListISPs(r.Context())
		out["isps"] = nz(isps)
	}
	writeJSON(w, 200, out)
}
func (s *Server) handleCaseGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	n, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil {
		s.jsonErr(w, store.ErrNotFound)
		return
	}
	c, e := s.store.GetInvestigation(r.Context(), n)
	if e != nil || (!id.isDirector() && c.ISPID != id.ISPID) {
		s.jsonErr(w, store.ErrNotFound)
		return
	}
	writeJSON(w, 200, c)
}
func (s *Server) handleCaseSave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var c store.Investigation
	if e := json.NewDecoder(r.Body).Decode(&c); e != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if raw := r.PathValue("id"); raw != "" {
		c.ID, _ = strconv.ParseInt(raw, 10, 64)
	}
	c.Reference = strings.TrimSpace(c.Reference)
	c.Title = strings.TrimSpace(c.Title)
	c.Notes = strings.TrimSpace(c.Notes)
	c.Status = strings.ToLower(strings.TrimSpace(c.Status))
	if c.Status == "" {
		c.Status = "open"
	}
	if c.Reference == "" || c.Title == "" || (c.Status != "open" && c.Status != "closed") {
		writeJSON(w, 422, map[string]string{"error": "reference, title and valid status are required"})
		return
	}
	if len(c.Reference) > 190 || len(c.Title) > 255 || len(c.Notes) > 1<<20 {
		writeJSON(w, 422, map[string]string{"error": "case fields exceed limits"})
		return
	}
	if c.ID > 0 {
		old, e := s.store.GetInvestigation(r.Context(), c.ID)
		if e != nil || (!id.isDirector() && old.ISPID != id.ISPID) {
			s.jsonErr(w, store.ErrNotFound)
			return
		}
		c.ISPID = old.ISPID
		c.CreatedBy = old.CreatedBy
	} else {
		scope, e := id.scopeISP(c.ISPID)
		if e != nil || scope == 0 {
			writeJSON(w, 403, map[string]string{"error": "choose a permitted ISP"})
			return
		}
		c.ISPID = scope
		c.CreatedBy = id.Email
	}
	saved, e := s.store.SaveInvestigation(r.Context(), c)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, saved)
}
