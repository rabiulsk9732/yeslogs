package crm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouterOSLookup(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "api" || p != "secret" {
			t.Error("missing auth")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"user":"alice","address":"10.0.0.8","calling-station-id":"AA:BB","session-id":"s1"}]`))
	}))
	defer ts.Close()
	c, e := NewRouterOSConnector(RouterOSConfig{Endpoint: ts.URL, Username: "api", Password: "secret", HTTPClient: ts.Client()})
	if e != nil {
		t.Fatal(e)
	}
	got, e := c.Lookup(context.Background(), []LookupRequest{{ReferenceCode: "x", LocalIP: "10.0.0.8", EventTime: time.Now()}})
	if e != nil || len(got) != 1 || got[0].Subscriber.Username != "alice" {
		t.Fatalf("%+v %v", got, e)
	}
}
