// Package director implements the NATFlow control-plane service: multi-tenant
// ISP/device management, an agent config-pull API the dataplane obeys, and
// ISP-scoped flow dashboards over ClickHouse.
package director

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/natflow/natflow-dataplane/internal/archive"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/director/agentcfg"
	"github.com/natflow/natflow-dataplane/internal/director/store"
)

//go:embed web/templates/*.html
var templatesFS embed.FS

const sessionCookie = "nf_session"
const flowWindowDays = 1

// Config configures the Director server.
type Config struct {
	SessionKey   []byte
	CookieSecure bool
	FlowDays     int
}

// Server is the Director HTTP service.
type Server struct {
	store      store.Store
	flows      *FlowReader // may be nil if ClickHouse is not configured
	tmpl       *template.Template
	sessionKey []byte
	secure     bool
	flowDays   int
	dummyHash  string // bcrypt hash used to equalize login timing for unknown users
	startedAt  time.Time
	log        *slog.Logger

	// retention / archive (optional; set via SetArchive / SetRetentionDays).
	// Guarded by archMu — written by the applier goroutine, read by handlers.
	archMu        sync.Mutex
	arch          *archive.Exporter
	archBucket    string
	archFormat    string
	retentionDays int
	cold          ColdS3 // S3 read-back config for cold (archived) search

	// editable settings (DB-backed source of truth; applied live via applier)
	settingsMu sync.Mutex
	settings   Settings
	applyMu    sync.Mutex     // serializes Apply() so concurrent saves apply in order
	applier    func(Settings) // set by the host (natlog) to apply changes to the dataplane
	statsFn    func() DPStats // dataplane stats snapshot (Overview/Dataplanes)

	// device-liveness alerting (optional; notifier set via SetNotifier).
	notifyMu sync.Mutex
	notifier Notifier
	healthMu sync.Mutex                 // guards health (written only by the monitor goroutine)
	health   map[string]*devHealthState // per-exporter liveness state

	// short-TTL cache for the (expensive) dashboard aggregates, per tenant.
	consoleMu    sync.Mutex
	consoleCache map[uint32]consoleCacheEntry
}

// SetArchive enables the S3 cold-archive feature in the console.
func (s *Server) SetArchive(exp *archive.Exporter, bucket, format string) {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	s.arch, s.archBucket, s.archFormat = exp, bucket, format
}

// archInfo returns the current archive exporter + bucket/format (locked).
func (s *Server) archInfo() (*archive.Exporter, string, string) {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	return s.arch, s.archBucket, s.archFormat
}

// SetColdSource configures S3 read-back for searching archived (cold) days.
// An empty config (Endpoint=="") disables cold search.
func (s *Server) SetColdSource(c ColdS3) {
	s.archMu.Lock()
	s.cold = c
	s.archMu.Unlock()
}

func (s *Server) coldInfo() ColdS3 {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	return s.cold
}

// SetRetentionDays sets the displayed retention target (days).
func (s *Server) SetRetentionDays(d int) {
	s.archMu.Lock()
	s.retentionDays = d
	s.archMu.Unlock()
}

func (s *Server) retDays() int {
	s.archMu.Lock()
	defer s.archMu.Unlock()
	return s.retentionDays
}

// New builds a Server.
func New(cfg Config, st store.Store, fr *FlowReader, log *slog.Logger) (*Server, error) {
	if len(cfg.SessionKey) < 16 {
		return nil, fmt.Errorf("session key too short")
	}
	t, err := template.New("").Funcs(template.FuncMap{
		"bytes": humanBytes,
	}).ParseFS(templatesFS, "web/templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	days := cfg.FlowDays
	if days <= 0 {
		days = flowWindowDays
	}
	dummy, err := HashPassword("not-a-real-password-timing-equalizer")
	if err != nil {
		return nil, err
	}
	return &Server{store: st, flows: fr, tmpl: t, sessionKey: cfg.SessionKey, secure: cfg.CookieSecure, flowDays: days, dummyHash: dummy, startedAt: time.Now(), log: log}, nil
}

// Handler returns the HTTP handler with all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Agent API (token auth).
	mux.HandleFunc("GET /api/v1/agent/config", s.handleAgentConfig)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	// Console SPA + JSON API (session cookie; handlers do their own auth).
	mux.HandleFunc("GET /{$}", s.serveConsole)
	mux.Handle("GET /assets/", s.assetsHandler())
	mux.HandleFunc("GET /api/v1/me", s.handleMe)
	mux.HandleFunc("POST /api/v1/login", s.handleAPILogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleAPILogout)
	mux.HandleFunc("GET /api/v1/console/data", s.handleConsoleData)
	mux.HandleFunc("GET /api/v1/isps", s.apiListISPs)
	mux.HandleFunc("POST /api/v1/isps", s.apiCreateISP)
	mux.HandleFunc("POST /api/v1/isps/{id}/toggle", s.apiToggleISP)
	mux.HandleFunc("GET /api/v1/devices", s.apiListDevices)
	mux.HandleFunc("POST /api/v1/devices", s.apiCreateDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/toggle", s.apiToggleDevice)
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.apiDeleteDevice)
	mux.HandleFunc("POST /api/v1/search", s.handleSearch)
	mux.HandleFunc("GET /api/v1/report", s.handleReport)
	mux.HandleFunc("GET /api/v1/audit", s.handleAudit)
	mux.HandleFunc("GET /api/v1/retention", s.handleRetention)
	mux.HandleFunc("POST /api/v1/archive/sweep", s.handleArchiveSweep) // literal beats {date}
	mux.HandleFunc("POST /api/v1/archive/{date}", s.handleArchive)
	mux.HandleFunc("PUT /api/v1/devices/{id}", s.apiUpdateDevice)
	mux.HandleFunc("GET /api/v1/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/v1/settings/{section}", s.handlePutSettings)
	mux.HandleFunc("POST /api/v1/notifications/test", s.handleTestNotification)
	mux.HandleFunc("GET /api/v1/system", s.handleSystem)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/dataplanes", s.handleDataplanes)
	mux.HandleFunc("GET /api/v1/users", s.apiListUsers)
	mux.HandleFunc("POST /api/v1/users", s.apiCreateUser)
	mux.HandleFunc("DELETE /api/v1/users/{id}", s.apiDeleteUser)
	mux.HandleFunc("POST /api/v1/users/{id}/reset", s.apiResetPassword)
	mux.HandleFunc("POST /api/v1/account/password", s.apiChangeOwnPassword)
	mux.HandleFunc("GET /api/v1/policies", s.apiListPolicies)
	mux.HandleFunc("POST /api/v1/policies", s.apiCreatePolicy)
	mux.HandleFunc("DELETE /api/v1/policies/{id}", s.apiDeletePolicy)

	// Legacy server-rendered admin pages (session auth).
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.Handle("POST /logout", s.auth(s.handleLogout))
	mux.Handle("GET /admin", s.auth(s.handleHome))
	mux.Handle("GET /isps", s.director(s.handleISPs))
	mux.Handle("POST /isps", s.director(s.handleCreateISP))
	mux.Handle("POST /isps/{id}/toggle", s.director(s.handleToggleISP))
	mux.Handle("GET /devices", s.auth(s.handleDevices))
	mux.Handle("POST /devices", s.auth(s.handleCreateDevice))
	mux.Handle("POST /devices/{id}/delete", s.auth(s.handleDeleteDevice))
	mux.Handle("POST /devices/{id}/toggle", s.auth(s.handleToggleDevice))
	mux.Handle("GET /flows", s.auth(s.handleFlows))
	return mux
}

// --- middleware ---

func (s *Server) auth(h func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		id, ok := s.parseSession(c.Value)
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// CSRF: state-changing requests must carry the session-bound token.
		if r.Method == http.MethodPost && !s.validCSRF(id, r.FormValue("csrf")) {
			s.render(w, r, "error", id, http.StatusForbidden, "invalid or missing CSRF token", nil)
			return
		}
		h(w, r, id)
	})
}

func (s *Server) director(h func(http.ResponseWriter, *http.Request, Identity)) http.Handler {
	return s.auth(func(w http.ResponseWriter, r *http.Request, id Identity) {
		if !id.isDirector() {
			s.render(w, r, "error", id, http.StatusForbidden, "forbidden", nil)
			return
		}
		h(w, r, id)
	})
}

// --- view helpers ---

type view struct {
	ID    Identity
	Title string
	Err   string
	CSRF  string
	Data  any
}

func (s *Server) render(w http.ResponseWriter, _ *http.Request, name string, id Identity, status int, errMsg string, data any) {
	v := view{ID: id, Title: name, Err: errMsg, Data: data}
	if id.Email != "" {
		v.CSRF = s.csrfToken(id)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("template render failed", "name", name, "error", err)
	}
}

func (s *Server) setSession(w http.ResponseWriter, id Identity) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.signSession(id), Path: "/",
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
		Expires: time.Unix(id.Exp, 0),
	})
}

// --- auth handlers ---

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", Identity{}, http.StatusOK, "", nil)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(strings.ToLower(r.FormValue("email")))
	pw := r.FormValue("password")
	u, err := s.store.GetUserByEmail(r.Context(), email)
	// Always run bcrypt (against a dummy hash for unknown users) so the response
	// time does not reveal whether the account exists.
	hash := s.dummyHash
	if err == nil {
		hash = u.PasswordHash
	}
	if !VerifyPassword(hash, pw) || err != nil {
		s.render(w, r, "login", Identity{}, http.StatusUnauthorized, "invalid credentials", nil)
		return
	}
	if !s.ispLoginAllowed(r.Context(), u) {
		s.render(w, r, "login", Identity{}, http.StatusForbidden, "this ISP account is disabled", nil)
		return
	}
	id := Identity{UserID: u.ID, ISPID: u.ISPID, Role: u.Role, Email: u.Email, Exp: time.Now().Add(sessionTTL).Unix()}
	s.setSession(w, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ispLoginAllowed reports whether the user may sign in: directors always may; an
// ISP user may only if their tenant exists and is enabled. (Disabling an ISP
// blocks new logins; existing stateless sessions expire within sessionTTL.)
func (s *Server) ispLoginAllowed(ctx context.Context, u store.User) bool {
	if u.ISPID == 0 {
		return true
	}
	isp, err := s.store.GetISP(ctx, u.ISPID)
	return err == nil && isp.Enabled
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, _ Identity) {
	// Mirror the attributes used when the cookie was set.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- home ---

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request, id Identity) {
	data := map[string]any{}
	if devs, err := s.store.ListDevices(r.Context(), tenantScope(id)); err == nil {
		data["Devices"] = len(devs)
	}
	if id.isDirector() {
		if isps, err := s.store.ListISPs(r.Context()); err == nil {
			data["ISPs"] = len(isps)
		}
	}
	if s.flows != nil {
		if sum, err := s.flows.Summary(r.Context(), id.ISPID, s.flowDays); err == nil {
			data["Flows"] = sum
		}
	}
	s.render(w, r, "home", id, http.StatusOK, "", data)
}

// tenantScope returns the isp filter for list queries (0 = all for director).
func tenantScope(id Identity) uint32 {
	if id.isDirector() {
		return 0
	}
	return id.ISPID
}

// --- ISP handlers (director only) ---

func (s *Server) handleISPs(w http.ResponseWriter, r *http.Request, id Identity) {
	isps, err := s.store.ListISPs(r.Context())
	if err != nil {
		s.log.Error("list isps failed", "error", err)
		s.render(w, r, "error", id, http.StatusInternalServerError, "internal error", nil)
		return
	}
	s.render(w, r, "isps", id, http.StatusOK, r.URL.Query().Get("err"), isps)
}

func (s *Server) handleCreateISP(w http.ResponseWriter, r *http.Request, id Identity) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/isps?err=name+required", http.StatusSeeOther)
		return
	}
	isp, err := s.store.CreateISP(r.Context(), name)
	if err != nil {
		if !errors.Is(err, store.ErrDuplicate) {
			s.log.Error("create isp failed", "error", err)
		}
		http.Redirect(w, r, "/isps?err="+escErr(err), http.StatusSeeOther)
		return
	}
	// Optionally create the ISP's first admin login.
	email := strings.TrimSpace(strings.ToLower(r.FormValue("admin_email")))
	pw := r.FormValue("admin_password")
	if email != "" && pw != "" {
		hash, herr := HashPassword(pw)
		if herr == nil {
			_, _ = s.store.CreateUser(r.Context(), store.User{ISPID: isp.ID, Email: email, PasswordHash: hash, Role: store.RoleISP})
		}
	}
	http.Redirect(w, r, "/isps", http.StatusSeeOther)
}

func (s *Server) handleToggleISP(w http.ResponseWriter, r *http.Request, _ Identity) {
	id64, _ := strconv.ParseUint(r.PathValue("id"), 10, 32)
	isp, err := s.store.GetISP(r.Context(), uint32(id64))
	if err != nil {
		http.Redirect(w, r, "/isps?err=not+found", http.StatusSeeOther)
		return
	}
	_ = s.store.SetISPEnabled(r.Context(), isp.ID, !isp.Enabled)
	http.Redirect(w, r, "/isps", http.StatusSeeOther)
}

// --- device handlers (tenant-scoped) ---

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request, id Identity) {
	reqISP := parseUint32(r.URL.Query().Get("isp"))
	scope, err := id.scopeISP(reqISP)
	if err != nil {
		s.render(w, r, "error", id, http.StatusForbidden, "forbidden", nil)
		return
	}
	devs, err := s.store.ListDevices(r.Context(), scope)
	if err != nil {
		s.log.Error("list devices failed", "error", err)
		s.render(w, r, "error", id, http.StatusInternalServerError, "internal error", nil)
		return
	}
	data := map[string]any{"Devices": devs}
	if id.isDirector() {
		isps, _ := s.store.ListISPs(r.Context())
		data["ISPs"] = isps
	}
	s.render(w, r, "devices", id, http.StatusOK, r.URL.Query().Get("err"), data)
}

func (s *Server) handleCreateDevice(w http.ResponseWriter, r *http.Request, id Identity) {
	scope, err := id.scopeISP(parseUint32(r.FormValue("isp_id")))
	if err != nil || scope == 0 {
		http.Redirect(w, r, "/devices?err=pick+an+isp", http.StatusSeeOther)
		return
	}
	d := store.Device{
		ISPID:       scope,
		Name:        strings.TrimSpace(r.FormValue("name")),
		ExporterIP:  strings.TrimSpace(r.FormValue("exporter_ip")),
		DeviceID:    parseUint32(r.FormValue("device_id")),
		Protocol:    valOr(r.FormValue("protocol"), "auto"),
		Profile:     valOr(r.FormValue("profile"), "generic"),
		Enabled:     true,
		SkipDNS:     r.FormValue("skip_dns") == "on",
		SkipPrivate: r.FormValue("skip_private") == "on",
		SkipZero:    r.FormValue("skip_zero") == "on",
	}
	if _, err := s.store.GetISP(r.Context(), scope); err != nil {
		http.Redirect(w, r, "/devices?err=unknown+isp", http.StatusSeeOther)
		return
	}
	if err := device.Validate([]device.Spec{{
		Name: d.Name, ExporterIP: d.ExporterIP, ISPID: d.ISPID, DeviceID: d.DeviceID, Protocol: d.Protocol, Profile: d.Profile,
	}}); err != nil {
		// Validation messages are safe input feedback (no secrets).
		http.Redirect(w, r, "/devices?err="+escMsg("invalid device: check IP, device_id, protocol, profile"), http.StatusSeeOther)
		return
	}
	if _, err := s.store.CreateDevice(r.Context(), d); err != nil {
		if !errors.Is(err, store.ErrDuplicate) {
			s.log.Error("create device failed", "error", err)
		}
		http.Redirect(w, r, "/devices?err="+escErr(err), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// ownedDevice fetches a device and enforces tenant ownership.
func (s *Server) ownedDevice(r *http.Request, id Identity) (store.Device, error) {
	devID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := s.store.GetDevice(r.Context(), devID)
	if err != nil {
		return store.Device{}, err
	}
	if !id.isDirector() && d.ISPID != id.ISPID {
		return store.Device{}, store.ErrNotFound // do not reveal cross-tenant existence
	}
	return d, nil
}

func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request, id Identity) {
	d, err := s.ownedDevice(r, id)
	if err != nil {
		http.Redirect(w, r, "/devices?err=not+found", http.StatusSeeOther)
		return
	}
	_ = s.store.DeleteDevice(r.Context(), d.ID)
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

func (s *Server) handleToggleDevice(w http.ResponseWriter, r *http.Request, id Identity) {
	d, err := s.ownedDevice(r, id)
	if err != nil {
		http.Redirect(w, r, "/devices?err=not+found", http.StatusSeeOther)
		return
	}
	d.Enabled = !d.Enabled
	_ = s.store.UpdateDevice(r.Context(), d)
	http.Redirect(w, r, "/devices", http.StatusSeeOther)
}

// --- flows dashboard ---

func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request, id Identity) {
	if s.flows == nil {
		s.render(w, r, "flows", id, http.StatusOK, "ClickHouse not configured", nil)
		return
	}
	scope, err := id.scopeISP(parseUint32(r.URL.Query().Get("isp")))
	if err != nil {
		s.render(w, r, "error", id, http.StatusForbidden, "forbidden", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	data := map[string]any{"Scope": scope, "Days": s.flowDays}
	if sum, err := s.flows.Summary(ctx, scope, s.flowDays); err == nil {
		data["Summary"] = sum
	}
	if t, err := s.flows.TopTalkers(ctx, scope, s.flowDays, 10); err == nil {
		data["Talkers"] = t
	}
	if pd, err := s.flows.PerDevice(ctx, scope, s.flowDays); err == nil {
		data["PerDevice"] = pd
	}
	if rec, err := s.flows.Recent(ctx, scope, s.flowDays, 25); err == nil {
		data["Recent"] = rec
	}
	s.render(w, r, "flows", id, http.StatusOK, "", data)
}

// --- agent config API (token auth) ---

func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" {
		http.Error(w, "missing token", http.StatusUnauthorized)
		return
	}
	a, err := s.store.GetAgentByToken(r.Context(), HashToken(tok))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = s.store.TouchAgent(r.Context(), a.ID, time.Now().UTC())

	bundle, err := s.buildBundle(r.Context())
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(bundle)
}

// Bundle returns the device registry + policy bundle (for in-process collectors
// in the unified single-service deployment).
func (s *Server) Bundle(ctx context.Context) (agentcfg.Bundle, error) {
	return s.buildBundle(ctx)
}

// buildBundle assembles the device registry + policy for managed collectors.
func (s *Server) buildBundle(ctx context.Context) (agentcfg.Bundle, error) {
	devs, err := s.store.ListDevices(ctx, 0)
	if err != nil {
		return agentcfg.Bundle{}, err
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].ExporterIP < devs[j].ExporterIP })
	out := make([]agentcfg.Device, 0, len(devs))
	for _, d := range devs {
		out = append(out, agentcfg.Device{
			Name: d.Name, ExporterIP: d.ExporterIP, ISPID: d.ISPID, DeviceID: d.DeviceID,
			Protocol: d.Protocol, Profile: d.Profile, Enabled: d.Enabled,
			SkipDNS: d.SkipDNS, SkipPrivate: d.SkipPrivate, SkipZero: d.SkipZero,
		})
	}
	mode := s.CurrentSettings().Dataplane.UnknownExporterMode
	if mode == "" {
		mode = "reject"
	}
	b := agentcfg.Bundle{UnknownExporterMode: mode, Devices: out}
	raw, _ := json.Marshal(out)
	sum := sha256.Sum256(raw)
	b.Version = hex.EncodeToString(sum[:8])
	return b, nil
}

// --- bootstrap helpers (used by cmd/director) ---

// EnsureAdmin creates an initial director user if none exist.
func EnsureAdmin(ctx context.Context, st store.Store, email, pw string) (created bool, err error) {
	n, err := st.CountUsers(ctx)
	if err != nil || n > 0 {
		return false, err
	}
	hash, err := HashPassword(pw)
	if err != nil {
		return false, err
	}
	_, err = st.CreateUser(ctx, store.User{Email: strings.ToLower(email), PasswordHash: hash, Role: store.RoleDirector})
	return err == nil, err
}

// --- small utils ---

func parseUint32(s string) uint32 {
	n, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	return uint32(n)
}
func valOr(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// escErr maps a store error to a safe, generic client message (no internal
// detail). Callers log the real error server-side.
func escErr(err error) string {
	if errors.Is(err, store.ErrDuplicate) {
		return url.QueryEscape("already exists (duplicate name or exporter IP)")
	}
	return url.QueryEscape("could not save (see server logs)")
}

// escMsg url-encodes a known-safe message (e.g. input-validation feedback).
func escMsg(s string) string { return url.QueryEscape(s) }
func humanBytes(b uint64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
