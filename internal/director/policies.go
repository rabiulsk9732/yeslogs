package director

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// Policy versions are content snapshots; no migration or collector restart is needed.
func policyVersion(p store.CapturePolicy) string {
	b, _ := json.Marshal(p)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

type policyDevice struct {
	ID              int64
	ISPID, DeviceID uint32
	Name            string
	Enabled         bool
	RulesMatch      bool
}
type policyView struct {
	store.CapturePolicy
	Version     string
	DeviceCount int
	CanEdit     bool
	CanDelete   bool
	Devices     []policyDevice `json:",omitempty"`
}

func policyViews(pols []store.CapturePolicy, devs []store.Device, id Identity, detail bool) []policyView {
	out := make([]policyView, 0, len(pols))
	for _, p := range pols {
		v := policyView{CapturePolicy: p, Version: policyVersion(p), CanEdit: id.isDirector() || p.ISPID == id.ISPID}
		for _, d := range devs {
			if (p.ISPID == 0 || p.ISPID == d.ISPID) && strings.EqualFold(p.Name, d.CapturePolicy) {
				v.DeviceCount++
				if detail && len(v.Devices) < 50 {
					v.Devices = append(v.Devices, policyDevice{d.ID, d.ISPID, d.DeviceID, d.Name, d.Enabled, d.SkipDNS == p.SkipDNS && d.SkipPrivate == p.SkipPrivate && d.SkipZero == p.SkipZero})
				}
			}
		}
		v.CanDelete = v.CanEdit && v.DeviceCount == 0
		out = append(out, v)
	}
	return out
}
func (s *Server) apiListPolicies(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	pols, e := s.store.ListPolicies(r.Context(), tenantScope(id))
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	devs, e := s.store.ListDevices(r.Context(), tenantScope(id))
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	out := map[string]any{"policies": policyViews(pols, devs, id, false), "isDirector": id.isDirector(), "editablePolicies": true}
	if id.isDirector() {
		isps, e := s.store.ListISPs(r.Context())
		if e != nil {
			s.jsonErr(w, e)
			return
		}
		out["isps"] = isps
	}
	writeJSON(w, 200, out)
}
func (s *Server) getPolicy(r *http.Request, id Identity, write bool) (store.CapturePolicy, error) {
	n, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if e != nil || n < 1 {
		return store.CapturePolicy{}, store.ErrNotFound
	}
	pols, e := s.store.ListPolicies(r.Context(), tenantScope(id))
	if e != nil {
		return store.CapturePolicy{}, e
	}
	for _, p := range pols {
		if p.ID == n && (!write || id.isDirector() || p.ISPID == id.ISPID) {
			return p, nil
		}
	}
	return store.CapturePolicy{}, store.ErrNotFound
}
func (s *Server) apiGetPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	p, e := s.getPolicy(r, id, false)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	devs, e := s.store.ListDevices(r.Context(), tenantScope(id))
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	writeJSON(w, 200, policyViews([]store.CapturePolicy{p}, devs, id, true)[0])
}

type policyForm struct {
	Name                           string
	ISPID                          *uint32
	SkipDNS, SkipPrivate, SkipZero *bool
	Version                        string
}

func readPolicyForm(w http.ResponseWriter, r *http.Request) (policyForm, error) {
	var b policyForm
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(&b); e != nil {
		return b, clientErr("invalid policy form")
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return b, clientErr("invalid policy form")
	}
	b.Name = strings.TrimSpace(b.Name)
	return b, nil
}
func (s *Server) policyError(w http.ResponseWriter, e error) {
	switch {
	case errors.Is(e, store.ErrConflict):
		writeJSON(w, 409, map[string]string{"error": "This policy changed. Close and reopen it before saving."})
	case errors.Is(e, store.ErrPolicyInUse):
		writeJSON(w, 409, map[string]string{"error": "Reassign linked devices before renaming or deleting this policy."})
	case errors.Is(e, store.ErrDuplicate):
		writeJSON(w, 409, map[string]any{"error": "This name conflicts with a policy in the same or overlapping scope.", "fields": map[string]string{"Name": "Choose a unique name for this scope and its global presets."}})
	default:
		s.jsonErr(w, e)
	}
}
func (s *Server) savePolicy(w http.ResponseWriter, r *http.Request, update bool) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	var old *store.CapturePolicy
	if update {
		p, e := s.getPolicy(r, id, true)
		if e != nil {
			s.jsonErr(w, e)
			return
		}
		old = &p
	}
	b, e := readPolicyForm(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	fields := map[string]string{}
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 64 || strings.ContainsFunc(b.Name, unicode.IsControl) {
		fields["Name"] = "Enter a policy name (up to 64 characters, without control characters)."
	}
	if b.SkipDNS == nil {
		fields["SkipDNS"] = "Choose Keep or Skip for DNS records."
	}
	if b.SkipPrivate == nil {
		fields["SkipPrivate"] = "Choose Keep or Skip for private-to-private records."
	}
	if b.SkipZero == nil {
		fields["SkipZero"] = "Choose Keep or Skip for zero-byte records."
	}
	if id.isDirector() && b.ISPID == nil {
		fields["ISPID"] = "Choose Global or an ISP."
	}
	if update && b.Version == "" {
		fields["Version"] = "Close and reopen this policy before saving."
	}
	if len(fields) > 0 {
		writeJSON(w, 422, map[string]any{"error": "Check the highlighted fields.", "fields": fields})
		return
	}
	scope := id.ISPID
	if id.isDirector() {
		scope = *b.ISPID
	} else if b.ISPID != nil && *b.ISPID != scope {
		writeJSON(w, 403, map[string]string{"error": "Cannot create or move a policy outside your ISP."})
		return
	}
	if old != nil {
		if b.Version != policyVersion(*old) {
			s.policyError(w, store.ErrConflict)
			return
		}
		if scope != old.ISPID {
			writeJSON(w, 422, map[string]any{"error": "Policy scope cannot change.", "fields": map[string]string{"ISPID": "Create a separate policy for another scope."}})
			return
		}
	}
	if scope != 0 {
		if _, e = s.store.GetISP(r.Context(), scope); e != nil {
			writeJSON(w, 422, map[string]any{"error": "Choose an existing ISP.", "fields": map[string]string{"ISPID": "Choose an existing ISP."}})
			return
		}
	}
	p := store.CapturePolicy{ISPID: scope, Name: b.Name, SkipDNS: *b.SkipDNS, SkipPrivate: *b.SkipPrivate, SkipZero: *b.SkipZero}
	saved, e := s.store.SavePolicy(r.Context(), old, &p)
	if e != nil {
		s.policyError(w, e)
		return
	}
	// Success does not depend on a second read after committing the write.
	writeJSON(w, 200, struct {
		store.CapturePolicy
		Version string
	}{saved, policyVersion(saved)})
}
func (s *Server) apiCreatePolicy(w http.ResponseWriter, r *http.Request) { s.savePolicy(w, r, false) }
func (s *Server) apiUpdatePolicy(w http.ResponseWriter, r *http.Request) { s.savePolicy(w, r, true) }
func (s *Server) apiDeletePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok || !s.csrfOK(w, r, id) {
		return
	}
	p, e := s.getPolicy(r, id, true)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	b, e := readPolicyForm(w, r)
	if e != nil {
		s.jsonErr(w, e)
		return
	}
	if b.Name != p.Name {
		writeJSON(w, 422, map[string]any{"error": "Enter the exact policy name to confirm.", "fields": map[string]string{"Name": "Enter the exact policy name to confirm."}})
		return
	}
	if b.Version == "" || b.Version != policyVersion(p) {
		s.policyError(w, store.ErrConflict)
		return
	}
	if _, e = s.store.SavePolicy(r.Context(), &p, nil); e != nil {
		s.policyError(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}
