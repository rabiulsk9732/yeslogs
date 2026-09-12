package director

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func caseRequest(t *testing.T, s *Server, method, path string, id Identity, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", s.csrfToken(id))
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestCasesAreTenantScopedAndRBACProtected(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	one, _ := st.CreateISP(ctx, "One")
	two, _ := st.CreateISP(ctx, "Two")
	director := dirIdentity()
	created := caseRequest(t, s, http.MethodPost, "/api/v1/cases", director, map[string]any{
		"ispId": one.ID, "reference": "FIR-001", "title": "Subscriber request", "status": "open", "notes": "Scoped evidence",
	})
	if created.Code != http.StatusOK {
		t.Fatalf("create case: %d %s", created.Code, created.Body.String())
	}
	var investigation store.Investigation
	if err := json.Unmarshal(created.Body.Bytes(), &investigation); err != nil {
		t.Fatal(err)
	}

	identity := func(role store.Role, isp uint32) Identity {
		return Identity{UserID: int64(isp) + 500, ISPID: isp, Role: role, Email: string(role) + "@example.invalid", Exp: time.Now().Add(time.Hour).Unix()}
	}
	manager := identity(store.RoleISP, one.ID)
	analyst := identity(store.RoleAnalyst, one.ID)
	auditor := identity(store.RoleAuditor, one.ID)
	other := identity(store.RoleISP, two.ID)

	for _, id := range []Identity{manager, analyst, auditor} {
		w := caseRequest(t, s, http.MethodGet, "/api/v1/cases", id, nil)
		if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte("FIR-001")) {
			t.Fatalf("permitted role %s could not view own case: %d %s", id.Role, w.Code, w.Body.String())
		}
	}
	w := caseRequest(t, s, http.MethodGet, "/api/v1/cases", other, nil)
	if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte("FIR-001")) {
		t.Fatalf("cross-tenant case leaked: %d %s", w.Code, w.Body.String())
	}
	w = caseRequest(t, s, http.MethodGet, "/api/v1/cases/"+jsonNumber(investigation.ID), other, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant case lookup returned %d", w.Code)
	}
	for _, id := range []Identity{analyst, auditor} {
		w = caseRequest(t, s, http.MethodPost, "/api/v1/cases", id, map[string]any{"ispId": one.ID, "reference": "NO", "title": "Denied"})
		if w.Code != http.StatusForbidden {
			t.Fatalf("read-only role %s mutated a case: %d", id.Role, w.Code)
		}
	}
	w = caseRequest(t, s, http.MethodPut, "/api/v1/cases/"+jsonNumber(investigation.ID), manager, map[string]any{
		"ispId": two.ID, "reference": "FIR-001", "title": "Updated", "status": "closed",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("manager update: %d %s", w.Code, w.Body.String())
	}
	got, err := st.GetInvestigation(ctx, investigation.ID)
	if err != nil || got.ISPID != one.ID || got.Status != "closed" {
		t.Fatalf("case scope changed during update: %+v %v", got, err)
	}
}

func TestAnalystCanReachAsyncExportButAuditorCannot(t *testing.T) {
	s, _ := testServer(t)
	identity := func(role store.Role) Identity {
		return Identity{UserID: 9, ISPID: 1, Role: role, Email: string(role) + "@example.invalid", Exp: time.Now().Add(time.Hour).Unix()}
	}
	analyst := caseRequest(t, s, http.MethodPost, "/api/v1/exports", identity(store.RoleAnalyst), map[string]any{})
	if analyst.Code != http.StatusServiceUnavailable {
		t.Fatalf("analyst was blocked before the export handler: %d %s", analyst.Code, analyst.Body.String())
	}
	auditor := caseRequest(t, s, http.MethodPost, "/api/v1/exports", identity(store.RoleAuditor), map[string]any{})
	if auditor.Code != http.StatusForbidden {
		t.Fatalf("auditor reached export handler: %d %s", auditor.Code, auditor.Body.String())
	}
}

func jsonNumber(n int64) string {
	return fmt.Sprintf("%d", n)
}
