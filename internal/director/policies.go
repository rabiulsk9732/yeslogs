package director

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// resolvePolicy returns the skip rules of a named capture policy in scope, if it
// exists. ok=false means "no such policy — use the device's own rules".
func (s *Server) resolvePolicy(ctx context.Context, scope uint32, name string) (skipDNS, skipPriv, skipZero, ok bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, false, false, false
	}
	pols, err := s.store.ListPolicies(ctx, scope)
	if err != nil {
		return false, false, false, false
	}
	for _, p := range pols {
		if p.Name == name {
			return p.SkipDNS, p.SkipPrivate, p.SkipZero, true
		}
	}
	return false, false, false, false
}

func (s *Server) apiListPolicies(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	pols, err := s.store.ListPolicies(r.Context(), tenantScope(id))
	if err != nil {
		s.log.Error("list policies", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": pols, "isDirector": id.isDirector()})
}

func (s *Server) apiCreatePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Name                           string
		ISPID                          uint32
		SkipDNS, SkipPrivate, SkipZero bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, clientErr("bad request"))
		return
	}
	// Directors may create global (ISPID 0) or tenant policies; ISP users get their own.
	scope := body.ISPID
	if !id.isDirector() {
		scope = id.ISPID
	}
	if strings.TrimSpace(body.Name) == "" {
		s.jsonErr(w, clientErr("name required"))
		return
	}
	p, err := s.store.CreatePolicy(r.Context(), store.CapturePolicy{
		ISPID: scope, Name: strings.TrimSpace(body.Name),
		SkipDNS: body.SkipDNS, SkipPrivate: body.SkipPrivate, SkipZero: body.SkipZero,
	})
	if err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) apiDeletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	pid, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	pols, err := s.store.ListPolicies(r.Context(), tenantScope(id))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var owned *store.CapturePolicy
	for i := range pols {
		if pols[i].ID == pid {
			owned = &pols[i]
			break
		}
	}
	// ISP users cannot delete global (ISPID 0) presets.
	if owned == nil || (!id.isDirector() && owned.ISPID != id.ISPID) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err := s.store.DeletePolicy(r.Context(), pid); err != nil {
		s.jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
