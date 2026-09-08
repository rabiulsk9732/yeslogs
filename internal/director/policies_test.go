package director

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"net/http"
	"net/http/httptest"
	"testing"
)

func policyBody(name string, scope uint32) map[string]any {
	return map[string]any{"Name": name, "ISPID": scope, "SkipDNS": false, "SkipPrivate": false, "SkipZero": false}
}
func TestPolicyCRUDScopeAndStoredRules(t *testing.T) {
	s, st := testServer(t)
	h := s.Handler()
	ctx := context.Background()
	a, _ := st.CreateISP(ctx, "Alpha")
	b, _ := st.CreateISP(ctx, "Beta")
	dir := dirIdentity()
	tenant := ispIdentity(a.ID)
	req := func(method, path string, id *Identity, body any, csrf bool, code int) *httptest.ResponseRecorder {
		t.Helper()
		w := ispRequest(s, h, method, path, id, body, csrf)
		if w.Code != code {
			t.Fatalf("%s %s: want %d got %d %s", method, path, code, w.Code, w.Body.String())
		}
		return w
	}
	req("POST", "/api/v1/policies", nil, policyBody("first", 0), false, 401)
	req("POST", "/api/v1/policies", &dir, policyBody("first", 0), false, 403)
	for _, key := range []string{"Name", "ISPID", "SkipDNS", "SkipPrivate", "SkipZero"} {
		body := policyBody("missing", 0)
		delete(body, key)
		req("POST", "/api/v1/policies", &dir, body, true, 422)
	}
	req("POST", "/api/v1/policies", &dir, policyBody("bad\nname", 0), true, 422)
	req("POST", "/api/v1/policies", &dir, policyBody("bad scope", 999), true, 422)
	req("POST", "/api/v1/policies", &tenant, policyBody("cross", b.ID), true, 403)
	create := func(name string, scope uint32, id *Identity) policyView {
		t.Helper()
		w := req("POST", "/api/v1/policies", id, policyBody(name, scope), true, 200)
		var v policyView
		json.Unmarshal(w.Body.Bytes(), &v)
		w = req("GET", fmt.Sprint("/api/v1/policies/", v.ID), id, nil, false, 200)
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	global := create("keep-all", 0, &dir)
	own := create("tenant-rules", a.ID, &tenant)
	other := create("tenant-rules", b.ID, &dir)
	req("POST", "/api/v1/policies", &tenant, policyBody("KEEP-ALL", a.ID), true, 409)
	req("POST", "/api/v1/policies", &dir, policyBody("tenant-rules", 0), true, 409)
	for _, p := range []policyView{global, other} {
		body := policyBody(p.Name, p.ISPID)
		body["Version"] = p.Version
		req("PUT", fmt.Sprint("/api/v1/policies/", p.ID), &tenant, body, true, 404)
		req("DELETE", fmt.Sprint("/api/v1/policies/", p.ID), &tenant, map[string]any{"Name": p.Name, "Version": p.Version}, true, 404)
	}
	d, _ := st.CreateDevice(ctx, store.Device{ISPID: a.ID, Name: "edge", DeviceID: 1, ExporterIP: "192.0.2.1", Enabled: true, CapturePolicy: own.Name})
	_, _ = st.CreateDevice(ctx, store.Device{ISPID: b.ID, Name: "private other tenant", DeviceID: 1, ExporterIP: "192.0.2.2", Enabled: true, CapturePolicy: global.Name})
	// A global preset is visible to ISP users, but other tenants' device usage is not.
	w := req("GET", fmt.Sprint("/api/v1/policies/", global.ID), &tenant, nil, false, 200)
	var visible policyView
	json.Unmarshal(w.Body.Bytes(), &visible)
	if visible.DeviceCount != 0 || visible.CanEdit || len(visible.Devices) > 0 {
		t.Fatal("global preset leaked usage or edit permission")
	}
	w = req("GET", "/api/v1/policies", &tenant, nil, false, 200)
	var list struct {
		Policies []policyView `json:"policies"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Policies) != 2 {
		t.Fatal("tenant list scope")
	}
	body := policyBody("renamed", own.ISPID)
	body["Version"] = own.Version
	req("PUT", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, body, true, 409)
	body["Name"] = own.Name
	body["SkipDNS"] = true
	req("PUT", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, body, true, 200)
	req("PUT", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, body, true, 409)
	got, _ := st.GetDevice(ctx, d.ID)
	if got.SkipDNS || !got.Enabled {
		t.Fatal("saving preset changed device capture configuration")
	}
	w = req("GET", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, nil, false, 200)
	json.Unmarshal(w.Body.Bytes(), &own)
	if own.DeviceCount != 1 || own.CanDelete || own.Devices[0].RulesMatch {
		t.Fatal("usage or drift not surfaced")
	}
	req("DELETE", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, map[string]any{"Name": own.Name, "Version": own.Version}, true, 409)
	_ = st.DeleteDevice(ctx, d.ID)
	req("DELETE", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, map[string]any{"Name": "wrong", "Version": own.Version}, true, 422)
	req("DELETE", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, map[string]any{"Name": own.Name, "Version": own.Version}, true, 200)
	req("GET", fmt.Sprint("/api/v1/policies/", own.ID), &tenant, nil, false, 404)
}
func TestManagementOwnsPolicyRoutesAndStillProxiesDevices(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	user, _ := st.CreateUser(ctx, store.User{Email: "director@example.invalid", Role: store.RoleDirector})
	id := dirIdentity()
	id.UserID = user.ID
	id.Email = user.Email
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls++; w.Write([]byte(`{"devices":[]}`)) }))
	defer upstream.Close()
	h, e := s.ManagementHandler(upstream.URL)
	if e != nil {
		t.Fatal(e)
	}
	for _, method := range []string{"GET", "POST"} {
		var body any
		if method == "POST" {
			body = policyBody("global", 0)
		}
		w := ispRequest(s, h, method, "/api/v1/policies", &id, body, true)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if upstreamCalls != 0 {
		t.Fatal("policies incorrectly forwarded")
	}
	w := ispRequest(s, h, "GET", "/api/v1/devices", &id, nil, false)
	if w.Code != 200 || upstreamCalls != 1 {
		t.Fatal("devices stopped reaching collector")
	}
}
