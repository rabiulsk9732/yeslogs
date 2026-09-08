package director

import (
	"context"
	"encoding/json"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRuntimeSettingsPreconditionsValidationAndForwarding(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	u, _ := st.CreateUser(ctx, store.User{Email: "director@example.invalid", Role: store.RoleDirector})
	id := dirIdentity()
	id.UserID = u.ID
	id.Email = u.Email
	cfg := map[string]any{"dataplane": map[string]any{"batchSize": 5000, "flushIntervalMs": 300, "writerWorkers": 2, "maxQueueRows": 200000, "backpressureMode": "block", "unknownExporterMode": "reject"}, "s3": map[string]any{"enabled": false, "endpoint": "", "bucket": "", "accessKey": "", "region": "", "pathPrefix": "", "secretKey": "masked", "exportFormat": "parquet", "autoArchive": false, "archiveAfterDays": 7}}
	writes := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, 200, map[string]any{"settings": cfg, "secretSet": true})
			return
		}
		writes++
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		cfg[strings.TrimPrefix(r.URL.Path, "/api/v1/settings/")] = b
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))
	defer up.Close()
	h, e := s.ManagementHandler(up.URL)
	if e != nil {
		t.Fatal(e)
	}
	get := func() map[string]string {
		w := ispRequest(s, h, "GET", "/api/v1/settings", &id, nil, false)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var v struct {
			Versions map[string]string `json:"versions"`
		}
		if json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatal("invalid decorated JSON")
		}
		return v.Versions
	}
	put := func(section, version string, b any, code int) {
		t.Helper()
		raw, _ := json.Marshal(b)
		r := httptest.NewRequest("PUT", "/api/v1/settings/"+section, strings.NewReader(string(raw)))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.signSession(id)})
		r.Header.Set("X-CSRF-Token", s.csrfToken(id))
		r.Header.Set("X-Settings-Version", version)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatalf("want %d got %d %s", code, w.Code, w.Body.String())
		}
	}
	version := get()["dataplane"]
	if len(version) != 64 {
		t.Fatal("missing version")
	}
	b := map[string]any{"batchSize": 6000, "flushIntervalMs": 300, "writerWorkers": 2, "maxQueueRows": 200000, "backpressureMode": "block", "unknownExporterMode": "reject"}
	put("dataplane", "", b, 428)
	put("dataplane", "stale", b, 409)
	b["batchSize"] = 1
	put("dataplane", version, b, 422)
	b["batchSize"] = 6000
	if writes != 0 {
		t.Fatal("invalid form reached collector")
	}
	put("dataplane", version, b, 200)
	if writes != 1 {
		t.Fatal("runtime write did not reach collector")
	}
	put("dataplane", version, b, 409)
	// An invisible credential rotation must also invalidate open settings forms.
	first := get()["s3"]
	if err := st.PutSetting(ctx, "s3", `{"secretKey":"new-test-secret"}`); err != nil {
		t.Fatal(err)
	}
	if get()["s3"] == first {
		t.Fatal("credential rotation did not change version")
	}
	if writes != 1 {
		t.Fatal("unexpected upstream mutation")
	}
}
func TestRuntimeSettingsFieldValidation(t *testing.T) {
	cases := []struct {
		section string
		body    map[string]any
		field   string
	}{
		{"dataplane", map[string]any{"batchSize": 1.5}, "batchSize"},
		{"skiprules", map[string]any{"skipDns": "false"}, "skipDns"},
		{"retention", map[string]any{"days": 0.0}, "days"},
		{"s3", map[string]any{"enabled": true, "endpoint": "file:///tmp/archive"}, "endpoint"},
		{"notifications", map[string]any{"enabled": true, "smtpHost": "smtp.example", "smtpUser": "invalid", "fromAddr": "", "recipients": "broken"}, "recipients"},
		{"crm", map[string]any{"enabled": true, "endpoint": "http://public.example/lookups"}, "endpoint"},
	}
	for _, tc := range cases {
		t.Run(tc.section, func(t *testing.T) {
			if validateSettingsForm(tc.section, tc.body)[tc.field] == "" {
				t.Fatal("invalid field accepted")
			}
		})
	}
}
