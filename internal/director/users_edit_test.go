package director

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsersCRUDVersionScopeAndPassword(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	h := s.Handler()
	dir := dirIdentity()
	dir.UserID = 999
	a, _ := st.CreateISP(ctx, "Alpha")
	b, _ := st.CreateISP(ctx, "Beta")
	tenant := ispIdentity(a.ID)
	req := func(method, path string, id *Identity, body any, csrf bool, code int) *httptest.ResponseRecorder {
		t.Helper()
		w := ispRequest(s, h, method, path, id, body, csrf)
		if w.Code != code {
			t.Fatalf("%s %s: want %d got %d %s", method, path, code, w.Code, w.Body.String())
		}
		return w
	}
	body := func(email string, scope uint32) map[string]any {
		return map[string]any{"Email": email, "ISPID": scope, "Role": "isp", "Password": "Test-password-123", "ConfirmPassword": "Test-password-123"}
	}
	req("POST", "/api/v1/users", nil, body("primary@alpha.example", a.ID), true, 401)
	req("POST", "/api/v1/users", &dir, body("primary@alpha.example", a.ID), false, 403)
	req("POST", "/api/v1/users", &tenant, body("other@beta.example", b.ID), true, 403)
	bad := body("bad@alpha.example", a.ID)
	bad["ConfirmPassword"] = "mismatch"
	req("POST", "/api/v1/users", &dir, bad, true, 422)
	for _, email := range []string{"primary@alpha.example", "operator@alpha.example"} {
		req("POST", "/api/v1/users", &dir, body(email, a.ID), true, 200)
	}
	req("POST", "/api/v1/users", &dir, body("other@beta.example", b.ID), true, 200)
	u, _ := st.GetUserByEmail(ctx, "operator@alpha.example")
	primary, _ := st.GetUserByEmail(ctx, "primary@alpha.example")
	path := fmt.Sprint("/api/v1/users/", u.ID)
	other, _ := st.GetUserByEmail(ctx, "other@beta.example")
	req("GET", fmt.Sprint("/api/v1/users/", other.ID), &tenant, nil, false, 404)
	w := req("GET", "/api/v1/users", &tenant, nil, false, 200)
	if strings.Contains(w.Body.String(), "beta.example") || strings.Contains(w.Body.String(), u.PasswordHash) {
		t.Fatal("tenant or credential leak")
	}
	req("DELETE", fmt.Sprint("/api/v1/users/", primary.ID), &dir, map[string]any{"Email": primary.Email, "Version": userVersion(primary)}, true, 409)
	edit := map[string]any{"Email": "renamed@alpha.example", "ISPID": a.ID, "Role": "isp", "Password": "", "ConfirmPassword": "", "Version": userVersion(u)}
	req("PUT", path, &tenant, edit, true, 200)
	saved, _ := st.GetUser(ctx, u.ID)
	if saved.PasswordHash != u.PasswordHash {
		t.Fatal("blank edit erased password")
	}
	req("PUT", path, &tenant, edit, true, 409)
	edit["Version"] = userVersion(saved)
	edit["Role"] = "director"
	req("PUT", path, &dir, edit, true, 422)
	edit["Role"] = "isp"
	edit["ISPID"] = b.ID
	req("PUT", path, &dir, edit, true, 422)
	reset := map[string]any{"Password": "Replacement-pass", "ConfirmPassword": "Replacement-pass", "Version": userVersion(saved)}
	req("POST", path+"/reset", &tenant, reset, true, 200)
	req("POST", path+"/reset", &tenant, reset, true, 409)
	own := tenant
	own.UserID = u.ID
	own.Email = saved.Email
	req("DELETE", path, &own, map[string]any{"Email": saved.Email}, true, 400)
	req("POST", path+"/reset", &own, reset, true, 400)
	change := map[string]any{"OldPassword": "wrong", "NewPassword": "Own-password-new", "ConfirmPassword": "Own-password-new"}
	req("POST", "/api/v1/account/password", &own, change, true, 422)
	change["OldPassword"] = "Replacement-pass"
	req("POST", "/api/v1/account/password", &own, change, true, 200)
	saved, _ = st.GetUser(ctx, u.ID)
	if !VerifyPassword(saved.PasswordHash, "Own-password-new") {
		t.Fatal("own password not applied")
	}
	req("DELETE", path, &tenant, map[string]any{"Email": "wrong@alpha.example", "Version": userVersion(saved)}, true, 422)
	req("DELETE", path, &tenant, map[string]any{"Email": saved.Email, "Version": userVersion(saved)}, true, 200)
	req("GET", path, &tenant, nil, false, 404)
}
func TestManagementOwnsAccountsAndProxiesRuntimeSettings(t *testing.T) {
	s, st := testServer(t)
	u, _ := st.CreateUser(context.Background(), store.User{Email: "director@example.invalid", Role: store.RoleDirector})
	id := dirIdentity()
	id.UserID = u.ID
	id.Email = u.Email
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"settings":{}}`)) }))
	defer up.Close()
	h, e := s.ManagementHandler(up.URL)
	if e != nil {
		t.Fatal(e)
	}
	w := ispRequest(s, h, "GET", "/api/v1/users", &id, nil, false)
	var data struct{ Users []userView }
	json.Unmarshal(w.Body.Bytes(), &data)
	if w.Code != 200 || len(data.Users) != 1 || calls != 0 {
		t.Fatal("accounts were proxied")
	}
	w = ispRequest(s, h, "GET", "/api/v1/settings", &id, nil, false)
	if w.Code != 200 || calls != 1 {
		t.Fatal("runtime settings stopped reaching collector")
	}
}
