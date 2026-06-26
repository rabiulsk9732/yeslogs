package director

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// JSON CRUD API for the console's ISP and Device management. All handlers do
// their own session auth; mutations require the session-bound CSRF token in the
// X-CSRF-Token header. Tenant scoping mirrors the server-rendered handlers.

func (s *Server) authJSON(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, ok := s.currentIdentity(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
	}
	return id, ok
}

func (s *Server) csrfOK(w http.ResponseWriter, r *http.Request, id Identity) bool {
	if !s.validCSRF(id, r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid CSRF token"})
		return false
	}
	return true
}

func jsonErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrDuplicate):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists (duplicate name or exporter IP)"})
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

// ---- ISPs (director only) ----

func (s *Server) apiListISPs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	isps, err := s.store.ListISPs(r.Context())
	if err != nil {
		s.log.Error("api list isps", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"isps": isps})
}

func (s *Server) apiCreateISP(w http.ResponseWriter, r *http.Request) {
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
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Name, AdminEmail, AdminPassword string
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, errors.New("bad request"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		jsonErr(w, errors.New("name required"))
		return
	}
	isp, err := s.store.CreateISP(r.Context(), name)
	if err != nil {
		jsonErr(w, err)
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.AdminEmail))
	if email != "" && body.AdminPassword != "" {
		if hash, herr := HashPassword(body.AdminPassword); herr == nil {
			_, _ = s.store.CreateUser(r.Context(), store.User{ISPID: isp.ID, Email: email, PasswordHash: hash, Role: store.RoleISP})
		}
	}
	writeJSON(w, http.StatusOK, isp)
}

func (s *Server) apiToggleISP(w http.ResponseWriter, r *http.Request) {
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
	id64, _ := strconv.ParseUint(r.PathValue("id"), 10, 32)
	isp, err := s.store.GetISP(r.Context(), uint32(id64))
	if err != nil {
		jsonErr(w, err)
		return
	}
	if err := s.store.SetISPEnabled(r.Context(), isp.ID, !isp.Enabled); err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": !isp.Enabled})
}

// ---- Devices (tenant-scoped) ----

func (s *Server) apiListDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	scope, err := id.scopeISP(parseUint32(r.URL.Query().Get("isp")))
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	devs, err := s.store.ListDevices(r.Context(), scope)
	if err != nil {
		s.log.Error("api list devices", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := map[string]any{"devices": devs, "isDirector": id.isDirector()}
	if id.isDirector() {
		if isps, e := s.store.ListISPs(r.Context()); e == nil {
			out["isps"] = isps
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiCreateDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		ISPID                          uint32
		Name, ExporterIP               string
		DeviceID                       uint32
		Protocol, Profile, Dataplane   string
		CapturePolicy                  string
		SkipDNS, SkipPrivate, SkipZero bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, errors.New("bad request"))
		return
	}
	scope, err := id.scopeISP(body.ISPID)
	if err != nil || scope == 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "pick a valid ISP"})
		return
	}
	if _, err := s.store.GetISP(r.Context(), scope); err != nil {
		jsonErr(w, errors.New("unknown ISP"))
		return
	}
	d := store.Device{
		ISPID: scope, Name: strings.TrimSpace(body.Name), ExporterIP: strings.TrimSpace(body.ExporterIP),
		DeviceID: body.DeviceID, Protocol: valOr(body.Protocol, "auto"), Profile: valOr(body.Profile, "generic"),
		CapturePolicy: strings.TrimSpace(body.CapturePolicy),
		Enabled:       true, SkipDNS: body.SkipDNS, SkipPrivate: body.SkipPrivate, SkipZero: body.SkipZero,
	}
	// A named capture policy supplies the device's skip rules.
	if sd, sp, sz, found := s.resolvePolicy(r.Context(), scope, d.CapturePolicy); found {
		d.SkipDNS, d.SkipPrivate, d.SkipZero = sd, sp, sz
	}
	if err := device.Validate([]device.Spec{{
		Name: d.Name, ExporterIP: d.ExporterIP, ISPID: d.ISPID, DeviceID: d.DeviceID, Protocol: d.Protocol, Profile: d.Profile,
	}}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid device: " + err.Error()})
		return
	}
	created, err := s.store.CreateDevice(r.Context(), d)
	if err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) apiUpdateDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	d, err := s.ownedDevice(r, id)
	if err != nil {
		jsonErr(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Name, ExporterIP               string
		DeviceID                       uint32
		Protocol, Profile              string
		CapturePolicy                  string
		SkipDNS, SkipPrivate, SkipZero bool
		Enabled                        bool
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, errors.New("bad request"))
		return
	}
	d.Name = strings.TrimSpace(body.Name)
	d.ExporterIP = strings.TrimSpace(body.ExporterIP)
	d.DeviceID = body.DeviceID
	d.Protocol = valOr(body.Protocol, "auto")
	d.Profile = valOr(body.Profile, "generic")
	d.CapturePolicy = strings.TrimSpace(body.CapturePolicy)
	d.SkipDNS, d.SkipPrivate, d.SkipZero = body.SkipDNS, body.SkipPrivate, body.SkipZero
	d.Enabled = body.Enabled
	if sd, sp, sz, found := s.resolvePolicy(r.Context(), d.ISPID, d.CapturePolicy); found {
		d.SkipDNS, d.SkipPrivate, d.SkipZero = sd, sp, sz
	}
	if err := device.Validate([]device.Spec{{
		Name: d.Name, ExporterIP: d.ExporterIP, ISPID: d.ISPID, DeviceID: d.DeviceID, Protocol: d.Protocol, Profile: d.Profile,
	}}); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid device: " + err.Error()})
		return
	}
	if err := s.store.UpdateDevice(r.Context(), d); err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) apiToggleDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	d, err := s.ownedDevice(r, id)
	if err != nil {
		jsonErr(w, err)
		return
	}
	d.Enabled = !d.Enabled
	if err := s.store.UpdateDevice(r.Context(), d); err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": d.Enabled})
}

func (s *Server) apiDeleteDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	d, err := s.ownedDevice(r, id)
	if err != nil {
		jsonErr(w, err)
		return
	}
	if err := s.store.DeleteDevice(r.Context(), d.ID); err != nil {
		jsonErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
