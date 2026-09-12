package crm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUniversalRESTConnector_BatchStandardFormat(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			Lookups []struct {
				ReferenceCode string `json:"referenceCode"`
				LocalIP       string `json:"localIp"`
				EventTime     string `json:"eventTime"`
			} `json:"lookups"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"schemaVersion": "yeslogs.crm.lookup.v1",
			"results": []map[string]any{
				{
					"referenceCode": req.Lookups[0].ReferenceCode,
					"status":        "matched",
					"subscriber": map[string]any{
						"accountId": "ACC-1001",
						"username":  "rajesh99",
						"name":      "Rajesh Sharma",
						"phone":     "9876543210",
						"address":   "Flat 4B, MG Road, Pune",
					},
					"session": map[string]any{
						"acctSessionId":    "SESS-9988",
						"callingStationId": "00:1A:2B:3C:4D:5E",
						"nasIpAddress":     "10.0.0.1",
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := RESTConfig{
		Endpoint:  ts.URL,
		AuthType:  AuthTypeBearer,
		AuthToken: "test-token",
		Timeout:   2 * time.Second,
		Format:    FormatYesLogsV1,
	}

	conn, err := NewRESTConnector(cfg)
	if err != nil {
		t.Fatalf("failed to create REST connector: %v", err)
	}
	defer conn.Close()

	reqs := []LookupRequest{
		{
			ReferenceCode: "REF-001",
			LocalIP:       "172.16.10.5",
			EventTime:     time.Now().UTC(),
		},
	}

	results, err := conn.Lookup(context.Background(), reqs)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	res := results[0]
	if res.Status != StatusMatched {
		t.Errorf("expected matched, got %s", res.Status)
	}
	if res.Subscriber.Name != "Rajesh Sharma" {
		t.Errorf("expected Rajesh Sharma, got %s", res.Subscriber.Name)
	}
	if res.Subscriber.Phone != "9876543210" {
		t.Errorf("expected 9876543210, got %s", res.Subscriber.Phone)
	}
	if res.Session.CallingStationID != "00:1A:2B:3C:4D:5E" {
		t.Errorf("expected MAC 00:1A:2B:3C:4D:5E, got %s", res.Session.CallingStationID)
	}
	if res.Subscriber.AccountID != "ACC-1001" {
		t.Errorf("expected ACC-1001, got %s", res.Subscriber.AccountID)
	}
}

func TestUniversalRESTConnector_CustomMapping(t *testing.T) {
	// Custom CRM returning arbitrary JSON structure
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"status": "success",
			"data": []map[string]any{
				{
					"lookup_id":     "REF-CUSTOM-1",
					"customer_name": "Anita Verma",
					"mobile_number": "9123456789",
					"mac_address":   "AA:BB:CC:DD:EE:FF",
					"user_login":    "anita_v",
					"home_address":  "Sector 14, Gurgaon",
					"cust_id":       "C-554",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := RESTConfig{
		Endpoint: ts.URL,
		Timeout:  2 * time.Second,
		Format:   FormatCustomJSON,
		FieldMapping: FieldMapping{
			ResultsArrayPath:   "data",
			ReferenceCodeField: "lookup_id",
			NameField:          "customer_name",
			PhoneField:         "mobile_number",
			MACField:           "mac_address",
			UsernameField:      "user_login",
			AddressField:       "home_address",
			AccountIDField:     "cust_id",
		},
	}

	conn, err := NewRESTConnector(cfg)
	if err != nil {
		t.Fatalf("failed to create REST connector: %v", err)
	}
	defer conn.Close()

	reqs := []LookupRequest{
		{
			ReferenceCode: "REF-CUSTOM-1",
			LocalIP:       "10.20.30.40",
			EventTime:     time.Now().UTC(),
		},
	}

	results, err := conn.Lookup(context.Background(), reqs)
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	res := results[0]
	if res.Status != StatusMatched {
		t.Errorf("expected matched, got %s", res.Status)
	}
	if res.Subscriber.Name != "Anita Verma" {
		t.Errorf("expected Anita Verma, got %s", res.Subscriber.Name)
	}
	if res.Subscriber.Phone != "9123456789" {
		t.Errorf("expected 9123456789, got %s", res.Subscriber.Phone)
	}
	if res.Session.CallingStationID != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("expected AA:BB:CC:DD:EE:FF, got %s", res.Session.CallingStationID)
	}
	if res.Subscriber.AccountID != "C-554" {
		t.Errorf("expected C-554, got %s", res.Subscriber.AccountID)
	}
}

func TestUniversalRESTConnector_TimeoutResilience(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := RESTConfig{
		Endpoint: ts.URL,
		Timeout:  50 * time.Millisecond, // very short timeout
		Format:   FormatYesLogsV1,
	}

	conn, err := NewRESTConnector(cfg)
	if err != nil {
		t.Fatalf("failed to create connector: %v", err)
	}
	defer conn.Close()

	reqs := []LookupRequest{
		{
			ReferenceCode: "REF-TIMEOUT",
			LocalIP:       "172.16.0.1",
			EventTime:     time.Now().UTC(),
		},
	}

	results, err := conn.Lookup(context.Background(), reqs)
	if err != nil {
		t.Fatalf("Lookup returned unexpected hard error instead of soft failure: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Status != StatusTimeout && results[0].Status != StatusError {
		t.Errorf("expected status timeout/error, got %s", results[0].Status)
	}
}

func TestCRMCache_TTLAndDeduplication(t *testing.T) {
	cache := NewCache(100, 100*time.Millisecond, 50*time.Millisecond)

	res := LookupResult{
		ReferenceCode: "REF-1",
		Status:        StatusMatched,
		Subscriber: SubscriberInfo{
			Name:  "Test User",
			Phone: "9999999999",
		},
		Session: SessionInfo{
			CallingStationID: "11:22:33:44:55:66",
		},
	}

	key := CacheKey(1, "172.16.1.1", time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC))

	// Cache miss initially
	if _, found := cache.Get(key); found {
		t.Fatal("expected cache miss initially")
	}

	// Put and get hit
	cache.Put(key, res)
	cached, found := cache.Get(key)
	if !found {
		t.Fatal("expected cache hit")
	}
	if cached.Subscriber.Name != "Test User" {
		t.Errorf("expected Test User, got %s", cached.Subscriber.Name)
	}

	// Wait for TTL expiration
	time.Sleep(120 * time.Millisecond)
	if _, found := cache.Get(key); found {
		t.Fatal("expected cache miss after TTL expiration")
	}
}
