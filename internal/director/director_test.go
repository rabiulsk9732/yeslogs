package director

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/agentcfg"
	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func testServer(t *testing.T) (*Server, *store.MemStore) {
	t.Helper()
	st := store.NewMem()
	s, err := New(Config{SessionKey: []byte("0123456789abcdef0123456789abcdef")}, st, nil, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func dirIdentity() Identity {
	return Identity{UserID: 1, Role: store.RoleDirector, Email: "admin@x.com", Exp: time.Now().Add(time.Hour).Unix()}
}
func ispIdentity(n uint32) Identity {
	return Identity{UserID: int64(n) + 100, ISPID: n, Role: store.RoleISP, Email: "isp@x.com", Exp: time.Now().Add(time.Hour).Unix()}
}

func (s *Server) do(method, target string, id *Identity, form url.Values) *httptest.ResponseRecorder {
	// Authenticated POSTs require the session-bound CSRF token.
	if method == http.MethodPost && id != nil {
		if form == nil {
			form = url.Values{}
		}
		if form.Get("csrf") == "" {
			form.Set("csrf", s.csrfToken(*id))
		}
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if id != nil {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(*id)})
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSessionRoundTripAndTamper(t *testing.T) {
	s, _ := testServer(t)
	id := dirIdentity()
	got, ok := s.parseSession(s.signSession(id))
	if !ok || got.UserID != id.UserID || got.Role != id.Role {
		t.Fatalf("session round-trip failed: %+v ok=%v", got, ok)
	}
	if _, ok := s.parseSession(s.signSession(id) + "x"); ok {
		t.Error("tampered session must be rejected")
	}
	if _, ok := s.parseSession("garbage"); ok {
		t.Error("garbage session must be rejected")
	}
}

func TestUnauthRedirects(t *testing.T) {
	s, _ := testServer(t)
	w := s.do("GET", "/devices", nil, nil)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Errorf("unauth should redirect to /login, got %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestLogin(t *testing.T) {
	s, st := testServer(t)
	hash, _ := HashPassword("s3cret")
	_, _ = st.CreateUser(context.Background(), store.User{Email: "admin@x.com", PasswordHash: hash, Role: store.RoleDirector})

	ok := s.do("POST", "/login", nil, url.Values{"email": {"admin@x.com"}, "password": {"s3cret"}})
	if ok.Code != http.StatusSeeOther || ok.Result().Cookies() == nil {
		t.Fatalf("valid login should set cookie + redirect, got %d", ok.Code)
	}
	bad := s.do("POST", "/login", nil, url.Values{"email": {"admin@x.com"}, "password": {"wrong"}})
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("bad login should be 401, got %d", bad.Code)
	}
}

func TestISPCreateRequiresDirector(t *testing.T) {
	s, st := testServer(t)
	isp := ispIdentity(1)
	if w := s.do("POST", "/isps", &isp, url.Values{"name": {"Hack"}}); w.Code != http.StatusForbidden {
		t.Errorf("isp user creating ISP should be 403, got %d", w.Code)
	}
	dir := dirIdentity()
	if w := s.do("POST", "/isps", &dir, url.Values{"name": {"Acme"}}); w.Code != http.StatusSeeOther {
		t.Errorf("director create ISP should redirect, got %d", w.Code)
	}
	if isps, _ := st.ListISPs(context.Background()); len(isps) != 1 {
		t.Errorf("expected 1 ISP, got %d", len(isps))
	}
}

func seedTwoISPsWithDevices(t *testing.T, st *store.MemStore) (a, b store.Device) {
	t.Helper()
	ctx := context.Background()
	i1, _ := st.CreateISP(ctx, "ISP1")
	i2, _ := st.CreateISP(ctx, "ISP2")
	a, _ = st.CreateDevice(ctx, store.Device{ISPID: i1.ID, Name: "a", ExporterIP: "203.0.113.1", DeviceID: 1, Protocol: "auto", Profile: "generic", Enabled: true})
	b, _ = st.CreateDevice(ctx, store.Device{ISPID: i2.ID, Name: "b", ExporterIP: "203.0.113.2", DeviceID: 2, Protocol: "auto", Profile: "generic", Enabled: true})
	return a, b
}

func TestDeviceListTenantScoped(t *testing.T) {
	s, st := testServer(t)
	a, b := seedTwoISPsWithDevices(t, st)
	isp1 := ispIdentity(a.ISPID)
	w := s.do("GET", "/devices", &isp1, nil)
	body := w.Body.String()
	if !strings.Contains(body, a.ExporterIP) {
		t.Error("isp1 should see its own device")
	}
	if strings.Contains(body, b.ExporterIP) {
		t.Error("TENANT BLEED: isp1 must NOT see isp2's device")
	}
}

func TestDeviceDeleteCrossTenantBlocked(t *testing.T) {
	s, st := testServer(t)
	a, b := seedTwoISPsWithDevices(t, st)
	isp1 := ispIdentity(a.ISPID)
	// isp1 tries to delete isp2's device by id.
	s.do("POST", "/devices/"+strconv.FormatInt(b.ID, 10)+"/delete", &isp1, url.Values{})
	if _, err := st.GetDevice(context.Background(), b.ID); err != nil {
		t.Fatal("cross-tenant delete succeeded — isp2 device was removed by isp1")
	}
	// isp1 deleting its OWN device works.
	s.do("POST", "/devices/"+strconv.FormatInt(a.ID, 10)+"/delete", &isp1, url.Values{})
	if _, err := st.GetDevice(context.Background(), a.ID); err == nil {
		t.Error("isp1 should be able to delete its own device")
	}
}

func TestDeviceCreateTenantEnforced(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	i1, _ := st.CreateISP(ctx, "ISP1")
	i2, _ := st.CreateISP(ctx, "ISP2")
	isp1 := ispIdentity(i1.ID)

	// isp1 explicitly passing isp_id=i2 must be rejected (nothing created).
	s.do("POST", "/devices", &isp1, url.Values{
		"isp_id": {strconv.FormatUint(uint64(i2.ID), 10)}, "name": {"x"},
		"exporter_ip": {"203.0.113.9"}, "device_id": {"7"},
	})
	if devs, _ := st.ListDevices(ctx, 0); len(devs) != 0 {
		t.Fatalf("cross-tenant create must be rejected, but %d device(s) created", len(devs))
	}

	// Normal create (no isp_id submitted) is scoped to the user's own ISP.
	s.do("POST", "/devices", &isp1, url.Values{
		"name": {"y"}, "exporter_ip": {"203.0.113.10"}, "device_id": {"8"},
	})
	devs, _ := st.ListDevices(ctx, 0)
	if len(devs) != 1 || devs[0].ISPID != i1.ID {
		t.Fatalf("own-tenant create wrong: %+v", devs)
	}
}

func TestAgentConfig(t *testing.T) {
	s, st := testServer(t)
	seedTwoISPsWithDevices(t, st)
	tok, _ := NewToken()
	_, _ = st.CreateAgent(context.Background(), "dp1", HashToken(tok))

	// No token -> 401.
	if w := s.do("GET", "/api/v1/agent/config", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("no token should be 401, got %d", w.Code)
	}
	// Bad token -> 401.
	r := httptest.NewRequest("GET", "/api/v1/agent/config", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("bad token should be 401, got %d", w.Code)
	}
	// Good token -> bundle with both devices and reject mode.
	r = httptest.NewRequest("GET", "/api/v1/agent/config", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("good token should be 200, got %d", w.Code)
	}
	var b agentcfg.Bundle
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Devices) != 2 || b.UnknownExporterMode != "reject" || b.Version == "" {
		t.Errorf("unexpected bundle: %+v", b)
	}
}

func TestCSRFRequired(t *testing.T) {
	s, st := testServer(t)
	a, _ := seedTwoISPsWithDevices(t, st)
	isp1 := ispIdentity(a.ISPID)
	// Authed POST with a WRONG csrf token must be rejected (403) and not delete.
	r := httptest.NewRequest("POST", "/devices/"+strconv.FormatInt(a.ID, 10)+"/delete",
		strings.NewReader(url.Values{"csrf": {"forged"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(isp1)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST without valid CSRF must be 403, got %d", w.Code)
	}
	if _, err := st.GetDevice(context.Background(), a.ID); err != nil {
		t.Fatal("CSRF-rejected request must not delete the device")
	}
}

func TestScopeISP(t *testing.T) {
	dir := dirIdentity()
	if v, _ := dir.scopeISP(5); v != 5 {
		t.Error("director should pass through requested isp")
	}
	if v, _ := dir.scopeISP(0); v != 0 {
		t.Error("director 0 = all")
	}
	isp := ispIdentity(3)
	if v, _ := isp.scopeISP(0); v != 3 {
		t.Error("isp scope defaults to own")
	}
	if _, err := isp.scopeISP(9); err == nil {
		t.Error("isp requesting another tenant must error")
	}
}
