package director

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func ispRequest(s *Server, h http.Handler, method, path string, id *Identity, body any, csrf bool) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	r.Header.Set("Content-Type", "application/json")
	if id != nil {
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(*id)})
		if csrf {
			r.Header.Set("X-CSRF-Token", s.csrfToken(*id))
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func validISPBody() map[string]any {
	return map[string]any{"Name": "Acme", "Username": "acme.admin", "Email": "admin@acme.example", "Phone": "+91 9876543210", "Password": "Test-only-password", "ConfirmPassword": "Test-only-password", "Enabled": true}
}
func TestISPRequiredFieldsAndAtomicCRUD(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler()
	dir := dirIdentity()
	tenant := ispIdentity(1)
	for _, tc := range []struct {
		id     *Identity
		csrf   bool
		status int
	}{{nil, false, 401}, {&tenant, true, 403}, {&dir, false, 403}} {
		w := ispRequest(s, h, "POST", "/api/v1/isps", tc.id, validISPBody(), tc.csrf)
		if w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, field := range []string{"Name", "Username", "Email", "Phone", "Password", "ConfirmPassword", "Enabled"} {
		b := validISPBody()
		delete(b, field)
		w := ispRequest(s, h, "POST", "/api/v1/isps", &dir, b, true)
		if w.Code != 422 {
			t.Fatalf("missing %s: %d %s", field, w.Code, w.Body.String())
		}
	}
	for field, value := range map[string]any{"Email": "Name <admin@example.com>", "Username": "bad@name", "Phone": "-------", "Password": strings.Repeat("a", 73), "ConfirmPassword": "wrong"} {
		b := validISPBody()
		b[field] = value
		w := ispRequest(s, h, "POST", "/api/v1/isps", &dir, b, true)
		if w.Code != 422 {
			t.Fatalf("invalid %s accepted", field)
		}
	}
	w := ispRequest(s, h, "POST", "/api/v1/isps", &dir, validISPBody(), true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var i store.ISP
	json.Unmarshal(w.Body.Bytes(), &i)
	u, _ := st.GetUserByLogin(context.Background(), "acme.admin")
	if !VerifyPassword(u.PasswordHash, "Test-only-password") {
		t.Fatal("password not hashed correctly")
	}
	w = ispRequest(s, h, "GET", "/api/v1/isps", &dir, nil, false)
	if strings.Contains(w.Body.String(), u.PasswordHash) || strings.Contains(w.Body.String(), "Test-only-password") {
		t.Fatal("password exposed")
	}
	b := validISPBody()
	b["Enabled"] = false
	b["Version"] = i.Version
	b["Password"] = ""
	b["ConfirmPassword"] = ""
	b["Name"] = "Updated"
	w = ispRequest(s, h, "PUT", "/api/v1/isps/1", &dir, b, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &i)
	w = ispRequest(s, h, "PUT", "/api/v1/isps/1", &dir, b, true)
	if w.Code != 409 {
		t.Fatal("stale update accepted")
	}
	w = ispRequest(s, h, "DELETE", "/api/v1/isps/1", &dir, map[string]any{"Name": "wrong", "Version": i.Version}, true)
	if w.Code != 400 {
		t.Fatal("delete confirmation ignored")
	}
	w = ispRequest(s, h, "DELETE", "/api/v1/isps/1", &dir, map[string]any{"Name": i.Name, "Version": i.Version}, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = ispRequest(s, h, "GET", "/api/v1/isps/1", &dir, nil, false)
	if w.Code != 404 {
		t.Fatal("deleted ISP visible")
	}
}
func TestManagementBridgeRevokesDisabledTenantAndPreservesRuntime(t *testing.T) {
	s, st := testServer(t)
	s.revalidateSessions = true
	ctx := context.Background()
	hash, _ := HashPassword("test-only-password")
	i, e := st.SaveISPAccount(ctx, store.ISP{Name: "Bridge", Username: "bridge", Email: "bridge@example.invalid", Phone: "1234567890", Enabled: true}, hash)
	if e != nil {
		t.Fatal(e)
	}
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls++; io.WriteString(w, "collector-runtime") }))
	defer upstream.Close()
	h, e := s.ManagementHandler(upstream.URL)
	if e != nil {
		t.Fatal(e)
	}
	w := ispRequest(s, h, "POST", "/api/v1/login", nil, map[string]string{"Email": "bridge", "Password": "test-only-password"}, false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var me meResp
	json.Unmarshal(w.Body.Bytes(), &me)
	if me.ISPID != i.ID {
		t.Fatal("username authenticated into wrong tenant")
	}
	id := Identity{UserID: i.AdminUserID, ISPID: i.ID, Role: store.RoleISP, Email: i.Email, Exp: time.Now().Add(time.Hour).Unix()}
	w = ispRequest(s, h, "GET", "/api/v1/overview", &id, nil, false)
	if w.Code != 200 || w.Body.String() != "collector-runtime" {
		t.Fatal("runtime was not forwarded")
	}
	if e = st.SetISPEnabled(ctx, i.ID, false); e != nil {
		t.Fatal(e)
	}
	w = ispRequest(s, h, "GET", "/api/v1/overview", &id, nil, false)
	if w.Code != 401 || upstreamCalls != 1 {
		t.Fatal("disabled cookie reached upstream")
	}
	w = ispRequest(s, h, "POST", "/api/v1/login", nil, map[string]string{"Email": "bridge", "Password": "test-only-password"}, false)
	if w.Code != 403 {
		t.Fatal("disabled ISP signed in")
	}
	w = ispRequest(s, h, "GET", "/api/v1/isps", &id, nil, false)
	if w.Code != 401 {
		t.Fatal("disabled account accessed directory")
	}
	if _, e = s.ManagementHandler("http://example.com:8080"); e == nil {
		t.Fatal("nonlocal upstream accepted")
	}
}
