package director

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func TestFlowGatewayOwnsReadsAndPreservesRuntime(t *testing.T) {
	s, _ := testServer(t)
	upCalls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upCalls++
		io.WriteString(w, "collector-runtime")
	}))
	defer up.Close()
	if _, err := s.ManagementFlowHandler(up.URL); err == nil {
		t.Fatal("flow gateway accepted a missing reader")
	}
	s.flows = &FlowReader{} // validation cases below do not execute database queries
	h, err := s.ManagementFlowHandler(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	dir := dirIdentity()
	for _, tc := range []struct {
		method, path string
		id           *Identity
		body         any
		csrf         bool
		status       int
	}{
		{"POST", "/api/v1/search", nil, map[string]any{}, false, 401},
		{"POST", "/api/v1/search", &dir, map[string]any{}, false, 403},
		{"POST", "/api/v1/search", &dir, map[string]any{}, true, 400},
		{"GET", "/api/v1/report", &dir, nil, false, 403},
		{"GET", "/api/v1/report?csrf=" + s.csrfToken(dir), &dir, nil, false, 400},
		{"PUT", "/api/v1/search", &dir, nil, false, 405},
	} {
		w := ispRequest(s, h, tc.method, tc.path, tc.id, tc.body, tc.csrf)
		if w.Code != tc.status {
			t.Fatalf("%s %s: got %d %s, want %d", tc.method, tc.path, w.Code, w.Body.String(), tc.status)
		}
	}
	isp := ispIdentity(1)
	w := ispRequest(s, h, "POST", "/api/v1/search", &isp, map[string]any{"ISPID": 2, "DeviceID": 8}, true)
	if w.Code != 403 {
		t.Fatal("cross-tenant search did not fail before querying")
	}
	w = ispRequest(s, h, "GET", "/api/v1/report?isp=2&device=8&csrf="+s.csrfToken(isp), &isp, nil, false)
	if w.Code != 403 {
		t.Fatal("cross-tenant export did not fail before querying")
	}
	if upCalls != 0 {
		t.Fatal("a flow read reached the old collector")
	}
	for _, path := range []string{"/api/v1/overview", "/api/v1/devices", "/api/v1/retention", "/api/v1/console/data"} {
		w := ispRequest(s, h, "GET", path, &dir, nil, false)
		if w.Code != 200 || w.Body.String() != "collector-runtime" {
			t.Fatalf("runtime route %s moved away from collector", path)
		}
	}
	w = ispRequest(s, h, "GET", "/api/v1/management-version", nil, nil, false)
	if !strings.Contains(w.Body.String(), `"flowReads":true`) {
		t.Fatal("version probe does not report active flow ownership")
	}
	// Existing gateways remain proxies unless the new capability is opted into.
	legacy, _ := s.ManagementHandler(up.URL)
	w = ispRequest(s, legacy, "POST", "/api/v1/search", &dir, map[string]any{}, true)
	if w.Body.String() != "collector-runtime" {
		t.Fatal("flow reads were enabled for a legacy gateway")
	}
}

func TestFlowGatewayRefreshesColdAndCRMSettingsWithoutApplyingRuntime(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	s.InitSettings(ctx, Settings{S3: S3Settings{Enabled: true, Endpoint: "https://bootstrap.invalid", Bucket: "bootstrap"}})
	applied := 0
	s.SetApplier(func(Settings) { applied++ })
	if err := s.refreshFlowReadSettings(ctx); err != nil || !s.coldInfo().enabled() {
		t.Fatal("bootstrap archive defaults were lost", err)
	}
	_ = st.PutSetting(ctx, "s3", `{"enabled":true,"endpoint":"https://archive.invalid","bucket":"archive","pathPrefix":"flows","secretKey":"test-secret","exportFormat":"csvgz"}`)
	sealed, err := s.marshalCRMConnectors([]CRMConnectorSettings{{ISPID: 5, Enabled: true, Endpoint: "http://127.0.0.1:8099/lookup", APIKey: "test-api-key"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutSetting(ctx, "crm", string(sealed))
	if err := s.refreshFlowReadSettings(ctx); err != nil {
		t.Fatal(err)
	}
	cold := s.coldInfo()
	crm := s.CurrentSettings().CRMConnectors
	if cold.Endpoint != "https://archive.invalid" || cold.Bucket != "archive" || cold.SecretKey != "test-secret" || cold.Format != "csvgz" {
		t.Fatal("persisted archive settings did not replace bootstrap")
	}
	if len(crm) != 1 || crm[0].ISPID != 5 || !crm[0].Enabled || crm[0].APIKey != "test-api-key" {
		t.Fatal("gateway did not decrypt persisted CRM using shared session key")
	}
	_ = st.PutSetting(ctx, "s3", `{"enabled":false}`)
	_ = st.PutSetting(ctx, "crm", `[]`)
	if err := s.refreshFlowReadSettings(ctx); err != nil || s.coldInfo().enabled() || len(s.CurrentSettings().CRMConnectors) != 0 {
		t.Fatal("persisted connector disable was not observed", err)
	}
	if applied != 0 {
		t.Fatal("read gateway applied collector runtime settings")
	}
}

type unavailableFlowSettingsStore struct{ store.Store }

func (unavailableFlowSettingsStore) GetSettings(context.Context) (map[string]string, error) {
	return nil, errors.New("settings database unavailable")
}

func TestFlowGatewaySettingsFailureDoesNotFallBackToOldReader(t *testing.T) {
	s, _ := testServer(t)
	s.flows = &FlowReader{}
	s.store = unavailableFlowSettingsStore{s.store}
	h, err := s.ManagementFlowHandler("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	dir := dirIdentity()
	w := ispRequest(s, h, "POST", "/api/v1/search", &dir, map[string]any{"DeviceID": 8}, true)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "settings unavailable") {
		t.Fatal("failed configuration produced an incomplete or stale search", w.Code, w.Body.String())
	}
}
