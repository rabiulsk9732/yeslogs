package director

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func seedCRMISP(t *testing.T, st *store.MemStore) (store.ISP, store.Device) {
	t.Helper()
	isp, err := st.CreateISP(context.Background(), "CRM ISP")
	if err != nil {
		t.Fatal(err)
	}
	dev, err := st.CreateDevice(context.Background(), store.Device{
		ISPID: isp.ID, Name: "nas-edge-01", ExporterIP: "198.51.100.10",
		DeviceID: 44, Protocol: "auto", Profile: "generic", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return isp, dev
}

func TestCRMEnrichmentBatchesDeduplicatesCachesAndMaps(t *testing.T) {
	var calls atomic.Int32
	var received crmBatchRequest
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer crm-secret" {
			t.Errorf("unexpected Authorization header %q", got)
		}
		if got := r.Header.Get("X-YesLogs-Schema"); got != crmSchemaVersion {
			t.Errorf("unexpected schema header %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		resp := crmBatchResponse{SchemaVersion: crmSchemaVersion, RequestID: received.RequestID}
		for _, q := range received.Lookups {
			result := crmResult{ReferenceCode: q.ReferenceCode, Status: "not_found"}
			if q.LocalIP == "172.16.18.10" {
				result.Status = "matched"
				result.Subscriber = crmSubscriber{
					AccountID: "C-900", Username: "radius-user", Name: "Sample Customer",
					Address: "Test Road, Pune", Phone: "9999999999",
				}
				result.Session.AcctSessionID = "RAD-123"
			}
			resp.Results = append(resp.Results, result)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer crm.Close()

	s, st := testServer(t)
	isp, dev := seedCRMISP(t, st)
	s.InitSettings(context.Background(), Settings{CRMConnectors: []CRMConnectorSettings{{
		ISPID: isp.ID, Enabled: true, Endpoint: crm.URL, APIKey: "crm-secret",
		TimeoutMs: 2000, BatchSize: 100,
	}}})

	at := time.Date(2026, 7, 17, 8, 16, 19, 0, time.UTC)
	rows := []natRecord{
		{At: at, DevID: dev.DeviceID, PrivIP: "172.16.18.10", PrivPort: 1111},
		// Same subscriber IP/NAS/second is one RADIUS-session lookup, even though
		// multiple flow rows have different transport ports.
		{At: at, DevID: dev.DeviceID, PrivIP: "172.16.18.10", PrivPort: 2222},
		{At: at, DevID: dev.DeviceID, PrivIP: "172.16.18.11", PrivPort: 3333},
	}
	sum := s.enrichCRMRows(context.Background(), isp.ID, rows)
	if calls.Load() != 1 {
		t.Fatalf("expected one batch request, got %d", calls.Load())
	}
	if len(received.Lookups) != 2 {
		t.Fatalf("expected two deduplicated lookups, got %d", len(received.Lookups))
	}
	for _, q := range received.Lookups {
		if q.DeviceID != dev.DeviceID || q.NASIdentifier != dev.Name || q.NASIPAddress != dev.ExporterIP {
			t.Fatalf("missing NAS identity: %+v", q)
		}
		if _, err := time.Parse(time.RFC3339, q.EventTime); err != nil {
			t.Fatalf("eventTime is not RFC3339: %q", q.EventTime)
		}
	}
	if !sum.Enabled || sum.Status != "complete" || sum.Matched != 2 || sum.NotFound != 1 || sum.UniqueLookups != 2 {
		t.Fatalf("unexpected enrichment summary: %+v", sum)
	}
	if rows[0].CRMStatus != "matched" || rows[0].CRMUsername != "radius-user" ||
		rows[0].CRMName != "Sample Customer" || rows[0].CRMPhone != "9999999999" ||
		rows[0].CRMAccountID != "C-900" || rows[0].CRMSessionID != "RAD-123" {
		t.Fatalf("subscriber result not mapped: %+v", rows[0])
	}
	if rows[0].CRMReference == "" || rows[0].CRMReference != rows[1].CRMReference {
		t.Fatalf("same session lookup should share a stable reference: %q / %q", rows[0].CRMReference, rows[1].CRMReference)
	}
	if rows[2].CRMStatus != "not_found" {
		t.Fatalf("not-found result not preserved: %+v", rows[2])
	}

	second := append([]natRecord(nil), rows...)
	for i := range second {
		second[i].CRMStatus = ""
	}
	cached := s.enrichCRMRows(context.Background(), isp.ID, second)
	if calls.Load() != 1 {
		t.Fatalf("second enrichment should use cache, calls=%d", calls.Load())
	}
	if cached.Cached != 3 || cached.Matched != 2 || cached.NotFound != 1 {
		t.Fatalf("unexpected cached summary: %+v", cached)
	}
}

func TestCRMFailureDoesNotLoseIPDRRows(t *testing.T) {
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary outage", http.StatusServiceUnavailable)
	}))
	defer crm.Close()

	s, st := testServer(t)
	isp, dev := seedCRMISP(t, st)
	s.InitSettings(context.Background(), Settings{CRMConnectors: []CRMConnectorSettings{{
		ISPID: isp.ID, Enabled: true, Endpoint: crm.URL, APIKey: "secret",
		TimeoutMs: 1000, BatchSize: 100,
	}}})
	rows := []natRecord{{
		At: time.Now(), DevID: dev.DeviceID, PrivIP: "172.16.20.1",
		PubIP: "103.53.30.147", DstIP: "8.8.8.8",
	}}
	sum := s.enrichCRMRows(context.Background(), isp.ID, rows)
	if sum.Status != "unavailable" || sum.Failed != 1 || rows[0].CRMStatus != "error" {
		t.Fatalf("unexpected outage result: sum=%+v row=%+v", sum, rows[0])
	}
	if rows[0].PrivIP != "172.16.20.1" || rows[0].PubIP != "103.53.30.147" || rows[0].DstIP != "8.8.8.8" {
		t.Fatalf("CRM failure mutated the base IPDR row: %+v", rows[0])
	}
}

func TestCRMConnectorDoesNotRunWithoutExactTenantScope(t *testing.T) {
	var calls atomic.Int32
	crm := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer crm.Close()
	s, st := testServer(t)
	isp, _ := seedCRMISP(t, st)
	s.InitSettings(context.Background(), Settings{CRMConnectors: []CRMConnectorSettings{{
		ISPID: isp.ID, Enabled: true, Endpoint: crm.URL, APIKey: "secret",
	}}})
	rows := []natRecord{{At: time.Now(), DevID: 44, PrivIP: "172.16.1.1"}}
	sum := s.enrichCRMRows(context.Background(), 0, rows)
	if sum.Enabled || calls.Load() != 0 || rows[0].CRMStatus != "" {
		t.Fatalf("global director scope must not call a tenant CRM: sum=%+v calls=%d", sum, calls.Load())
	}
}

func TestCRMSecretsAreEncryptedAndRoundTrip(t *testing.T) {
	s, _ := testServer(t)
	const secret = "top-secret-bearer-value"
	raw, err := s.marshalCRMConnectors([]CRMConnectorSettings{{
		ISPID: 7, Enabled: true, Endpoint: "https://crm.example.test/v1/lookups",
		APIKey: secret, TimeoutMs: 5000, BatchSize: 500,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || !bytes.Contains(raw, []byte(crmSecretPrefix)) {
		t.Fatalf("CRM secret was not protected at rest: %s", raw)
	}
	got, err := s.unmarshalCRMConnectors(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].APIKey != secret {
		t.Fatalf("secret round-trip failed: %+v", got)
	}
}

func TestCRMSettingsAPIStoresEncryptedAndNeverEchoesKey(t *testing.T) {
	s, st := testServer(t)
	isp, _ := seedCRMISP(t, st)
	id := dirIdentity()
	payload, _ := json.Marshal(CRMConnectorSettings{
		ISPID: isp.ID, Enabled: true, Endpoint: "https://crm.example.test/v1/lookups",
		APIKey: "settings-api-secret", TimeoutMs: 5000, BatchSize: 500,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/crm", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrfToken(id))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("save CRM connector: status=%d body=%s", w.Code, w.Body.String())
	}
	persisted, err := st.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted["crm"], "settings-api-secret") || !strings.Contains(persisted["crm"], crmSecretPrefix) {
		t.Fatalf("persisted CRM credential is not encrypted: %s", persisted["crm"])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("read settings: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "settings-api-secret") {
		t.Fatal("settings API echoed the CRM API key")
	}
	var got struct {
		CRMKeySet map[string]bool `json:"crmKeySet"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.CRMKeySet[strconv.FormatUint(uint64(isp.ID), 10)] {
		t.Fatalf("settings API did not report a configured key: %s", w.Body.String())
	}
}

func TestCRMDoesNotChangeEightColumnReportAndMetadataRemainsSafe(t *testing.T) {
	meta := reportMeta{CaseRef: "=DANGEROUS()", CRM: crmEnrichmentSummary{Enabled: true, Matched: 1}}
	rows := []natRecord{{
		PrivIP: "172.16.1.10", PubIP: "203.0.113.10", Time: "2026-07-17 13:46:19",
		CRMReference: "YL-20260717-ABC123", CRMStatus: "matched", CRMUsername: "user1",
		CRMName: "=DANGEROUS()", CRMPhone: "9999999999", CRMAddress: "Pune",
	}}
	var out bytes.Buffer
	if err := writeCSV(&out, meta, rows); err != nil {
		t.Fatal(err)
	}
	csv := out.String()
	for _, want := range []string{"Source IP Address", "Translated IP address"} {
		if !strings.Contains(csv, want) {
			t.Errorf("CSV missing %q: %s", want, csv)
		}
	}
	for _, forbidden := range []string{"crm_reference", "subscriber_name", "YL-20260717-ABC123", "CRM enrichment"} {
		if strings.Contains(csv, forbidden) {
			t.Errorf("CRM changed requested eight-column table or leaked metadata into CSV: %s", csv)
		}
	}
	out.Reset()
	if err := writeXLSX(&out, meta, rows); err != nil || out.Len() < 500 {
		t.Fatalf("CRM XLSX generation failed: bytes=%d err=%v", out.Len(), err)
	}
	out.Reset()
	if err := writePDF(&out, meta, rows); err != nil || out.Len() < 500 {
		t.Fatalf("CRM PDF generation failed: bytes=%d err=%v", out.Len(), err)
	}
}

func TestCRMEndpointRequiresHTTPSOutsideLoopback(t *testing.T) {
	if _, err := sanitizeCRMConnector(CRMConnectorSettings{
		ISPID: 1, Enabled: true, Endpoint: "http://crm.example.test/lookups", APIKey: "x",
	}); err == nil {
		t.Fatal("non-loopback clear-text CRM endpoint must be rejected")
	}
}

func TestCRMTestEndpoint(t *testing.T) {
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req crmBatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := crmBatchResponse{
			SchemaVersion: crmSchemaVersion,
			RequestID:     req.RequestID,
			Results: []crmResult{
				{
					ReferenceCode: req.Lookups[0].ReferenceCode,
					Status:        "matched",
					Subscriber: crmSubscriber{
						AccountID: "ACC-55",
						Name:      "Test Subscriber",
						Phone:     "9876543210",
					},
					Session: crmSession{
						CallingStationID: "AA:BB:CC:11:22:33",
					},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer crm.Close()

	s, st := testServer(t)
	isp, _ := seedCRMISP(t, st)
	id := dirIdentity()

	// Configure connector
	payload, _ := json.Marshal(CRMConnectorSettings{
		ISPID: isp.ID, Enabled: true, Endpoint: crm.URL,
		APIKey: "secret", TimeoutMs: 5000, BatchSize: 500,
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/settings/crm", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", s.csrfToken(id))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("configure crm failed: %s", w.Body.String())
	}

	// Call test endpoint
	testBody, _ := json.Marshal(map[string]any{
		"ispId":   isp.ID,
		"localIp": "172.16.1.100",
	})
	testReq := httptest.NewRequest(http.MethodPost, "/api/v1/settings/crm/test", bytes.NewReader(testBody))
	testReq.Header.Set("Content-Type", "application/json")
	testReq.Header.Set("X-CSRF-Token", s.csrfToken(id))
	testReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
	testW := httptest.NewRecorder()
	s.Handler().ServeHTTP(testW, testReq)
	if testW.Code != http.StatusOK {
		t.Fatalf("test endpoint failed: %s", testW.Body.String())
	}

	var result struct {
		Status     string        `json:"status"`
		Subscriber crmSubscriber `json:"subscriber"`
		Session    crmSession    `json:"session"`
	}
	if err := json.Unmarshal(testW.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal test response: %v", err)
	}

	if result.Status != "matched" {
		t.Errorf("expected matched, got %s", result.Status)
	}
	if result.Subscriber.Name != "Test Subscriber" {
		t.Errorf("expected Test Subscriber, got %s", result.Subscriber.Name)
	}
	if result.Session.CallingStationID != "AA:BB:CC:11:22:33" {
		t.Errorf("expected CallingStationID AA:BB:CC:11:22:33, got %s", result.Session.CallingStationID)
	}
}

func TestDirectorRouterOSConnectorUsesConfiguredCredentials(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "api-user" || password != "api-password" {
			t.Errorf("unexpected RouterOS credentials: %q %q %v", user, password, ok)
		}
		if r.URL.Path != "/rest/user-manager/session" || r.URL.Query().Get("address") != "10.0.0.8" {
			t.Errorf("unexpected RouterOS request: %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"user":"alice","address":"10.0.0.8","calling-station-id":"AA:BB","session-id":"s1"}]`))
	}))
	defer ts.Close()
	s, _ := testServer(t)
	s.crmHTTP = ts.Client()
	got, err := s.sendCRMBatch(context.Background(), CRMConnectorSettings{
		Type: "routeros", Endpoint: ts.URL, Username: "api-user", APIKey: "api-password", TimeoutMs: 2000,
	}, []crmLookup{{ReferenceCode: "ref-1", LocalIP: "10.0.0.8", EventTime: time.Now().UTC().Format(time.RFC3339)}})
	if err != nil {
		t.Fatal(err)
	}
	if got["ref-1"].Status != "matched" || got["ref-1"].Subscriber.Username != "alice" || got["ref-1"].Session.CallingStationID != "AA:BB" {
		t.Fatalf("unexpected RouterOS result: %+v", got)
	}
}
