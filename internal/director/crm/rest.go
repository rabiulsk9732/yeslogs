package crm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type AuthType string

const (
	AuthTypeNone         AuthType = "none"
	AuthTypeBearer       AuthType = "bearer"
	AuthTypeCustomHeader AuthType = "custom_header"
	AuthTypeBasic        AuthType = "basic"
)

type RESTFormat string

const (
	FormatYesLogsV1  RESTFormat = "yeslogs_v1"
	FormatCustomJSON RESTFormat = "custom_json"
)

// FieldMapping allows mapping any arbitrary JSON structure to standard fields.
type FieldMapping struct {
	ResultsArrayPath   string `json:"resultsArrayPath,omitempty"`
	ReferenceCodeField string `json:"referenceCodeField,omitempty"`
	StatusField        string `json:"statusField,omitempty"`
	NameField          string `json:"nameField,omitempty"`
	PhoneField         string `json:"phoneField,omitempty"`
	EmailField         string `json:"emailField,omitempty"`
	AddressField       string `json:"addressField,omitempty"`
	UsernameField      string `json:"usernameField,omitempty"`
	AccountIDField     string `json:"accountIdField,omitempty"`
	MACField           string `json:"macField,omitempty"`
	SessionIDField     string `json:"sessionIdField,omitempty"`
}

// RESTConfig configures the universal HTTP REST connector.
type RESTConfig struct {
	Endpoint     string
	AuthType     AuthType
	AuthToken    string
	HeaderName   string
	Timeout      time.Duration
	Format       RESTFormat
	FieldMapping FieldMapping
	HTTPClient   *http.Client
}

// RESTConnector connects to any HTTP/REST CRM endpoint.
type RESTConnector struct {
	cfg        RESTConfig
	httpClient *http.Client
}

// NewRESTConnector constructs a RESTConnector.
func NewRESTConnector(cfg RESTConfig) (*RESTConnector, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil, errors.New("CRM endpoint URL cannot be empty")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse // disallow redirects for security
			},
		}
	}

	if cfg.Format == "" {
		cfg.Format = FormatYesLogsV1
	}

	return &RESTConnector{
		cfg:        cfg,
		httpClient: client,
	}, nil
}

// Close cleans up connector resources.
func (r *RESTConnector) Close() error {
	return nil
}

// Test performs a single lookup verification.
func (r *RESTConnector) Test(ctx context.Context, sample LookupRequest) (*LookupResult, error) {
	results, err := r.Lookup(ctx, []LookupRequest{sample})
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, errors.New("no result returned from CRM")
	}
	return &results[0], nil
}

// Lookup queries the remote CRM with a batch of lookups.
func (r *RESTConnector) Lookup(ctx context.Context, lookups []LookupRequest) ([]LookupResult, error) {
	if len(lookups) == 0 {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	if r.cfg.Format == FormatCustomJSON {
		return r.lookupCustomJSON(ctx, lookups)
	}

	return r.lookupYesLogsV1(ctx, lookups)
}

func randomRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "YLREQ-" + strings.ToUpper(hex.EncodeToString(b[:]))
}

func (r *RESTConnector) applyAuth(req *http.Request) {
	switch r.cfg.AuthType {
	case AuthTypeBearer:
		if r.cfg.AuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+r.cfg.AuthToken)
		}
	case AuthTypeCustomHeader:
		if r.cfg.HeaderName != "" && r.cfg.AuthToken != "" {
			req.Header.Set(r.cfg.HeaderName, r.cfg.AuthToken)
		}
	}
}

func (r *RESTConnector) lookupYesLogsV1(ctx context.Context, lookups []LookupRequest) ([]LookupResult, error) {
	reqID := randomRequestID()

	type v1Lookup struct {
		ReferenceCode string `json:"referenceCode"`
		LocalIP       string `json:"localIp"`
		EventTime     string `json:"eventTime"`
		DeviceID      uint32 `json:"deviceId,omitempty"`
		NASIdentifier string `json:"nasIdentifier,omitempty"`
		NASIPAddress  string `json:"nasIpAddress,omitempty"`
	}

	payloadLookups := make([]v1Lookup, len(lookups))
	for i, l := range lookups {
		payloadLookups[i] = v1Lookup{
			ReferenceCode: l.ReferenceCode,
			LocalIP:       l.LocalIP,
			EventTime:     l.EventTime.UTC().Format(time.RFC3339),
			DeviceID:      l.DeviceID,
			NASIdentifier: l.NASIdentifier,
			NASIPAddress:  l.NASIPAddress,
		}
	}

	reqBody := map[string]any{
		"schemaVersion": "yeslogs.crm.lookup.v1",
		"requestId":     reqID,
		"lookups":       payloadLookups,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-YesLogs-Schema", "yeslogs.crm.lookup.v1")
	httpReq.Header.Set("Idempotency-Key", reqID)
	r.applyAuth(httpReq)

	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		// Soft failure on timeout or connection error: return error results so flows still export
		results := make([]LookupResult, len(lookups))
		status := StatusError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
			status = StatusTimeout
		}
		for i, l := range lookups {
			results[i] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        status,
				ErrorMessage:  err.Error(),
			}
		}
		return results, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		results := make([]LookupResult, len(lookups))
		errMsg := fmt.Sprintf("CRM HTTP %d", resp.StatusCode)
		for i, l := range lookups {
			results[i] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        StatusError,
				ErrorMessage:  errMsg,
			}
		}
		return results, nil
	}

	var parsed struct {
		SchemaVersion string `json:"schemaVersion"`
		RequestID     string `json:"requestId"`
		Results       []struct {
			ReferenceCode string `json:"referenceCode"`
			Status        string `json:"status"`
			Subscriber    struct {
				AccountID string `json:"accountId"`
				Username  string `json:"username"`
				Name      string `json:"name"`
				Address   string `json:"address"`
				Phone     string `json:"phone"`
				Email     string `json:"email"`
			} `json:"subscriber"`
			Session struct {
				AcctSessionID    string `json:"acctSessionId"`
				CallingStationID string `json:"callingStationId"`
				NASIPAddress     string `json:"nasIpAddress"`
				NASPort          string `json:"nasPort"`
			} `json:"session"`
		} `json:"results"`
	}

	lr := io.LimitReader(resp.Body, 8<<20)
	if err := json.NewDecoder(lr).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("failed to decode CRM response: %w", err)
	}

	resultMap := make(map[string]LookupResult, len(parsed.Results))
	for _, res := range parsed.Results {
		st := StatusNotFound
		switch res.Status {
		case "matched":
			st = StatusMatched
		case "ambiguous":
			st = StatusAmbiguous
		case "error":
			st = StatusError
		}

		resultMap[res.ReferenceCode] = LookupResult{
			ReferenceCode: res.ReferenceCode,
			Status:        st,
			Subscriber: SubscriberInfo{
				AccountID: res.Subscriber.AccountID,
				Username:  res.Subscriber.Username,
				Name:      res.Subscriber.Name,
				Address:   res.Subscriber.Address,
				Phone:     res.Subscriber.Phone,
				Email:     res.Subscriber.Email,
			},
			Session: SessionInfo{
				AcctSessionID:    res.Session.AcctSessionID,
				CallingStationID: res.Session.CallingStationID,
				NASIPAddress:     res.Session.NASIPAddress,
				NASPort:          res.Session.NASPort,
			},
		}
	}

	results := make([]LookupResult, len(lookups))
	for i, l := range lookups {
		if r, ok := resultMap[l.ReferenceCode]; ok {
			results[i] = r
		} else {
			results[i] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        StatusNotFound,
			}
		}
	}

	return results, nil
}

func (r *RESTConnector) lookupCustomJSON(ctx context.Context, lookups []LookupRequest) ([]LookupResult, error) {
	reqID := randomRequestID()
	bodyBytes, err := json.Marshal(map[string]any{
		"requestId": reqID,
		"lookups":   lookups,
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.Endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	r.applyAuth(httpReq)

	resp, err := r.httpClient.Do(httpReq)
	if err != nil {
		results := make([]LookupResult, len(lookups))
		st := StatusError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
			st = StatusTimeout
		}
		for i, l := range lookups {
			results[i] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        st,
				ErrorMessage:  err.Error(),
			}
		}
		return results, nil
	}
	defer resp.Body.Close()

	var root any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&root); err != nil {
		return nil, err
	}

	// Navigate to results array
	items := extractArray(root, r.cfg.FieldMapping.ResultsArrayPath)
	m := r.cfg.FieldMapping

	refField := firstNonEmpty(m.ReferenceCodeField, "referenceCode", "id", "ref")
	nameField := firstNonEmpty(m.NameField, "name", "customer_name")
	phoneField := firstNonEmpty(m.PhoneField, "phone", "mobile")
	macField := firstNonEmpty(m.MACField, "mac", "callingStationId")
	userField := firstNonEmpty(m.UsernameField, "username", "user")
	addrField := firstNonEmpty(m.AddressField, "address", "addr")
	accField := firstNonEmpty(m.AccountIDField, "accountId", "account_id")

	resultMap := make(map[string]LookupResult)
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}

		ref := getString(itemMap, refField)
		if ref == "" {
			continue
		}

		name := getString(itemMap, nameField)
		phone := getString(itemMap, phoneField)
		mac := getString(itemMap, macField)
		user := getString(itemMap, userField)
		addr := getString(itemMap, addrField)
		acc := getString(itemMap, accField)

		status := StatusMatched
		if name == "" && user == "" && phone == "" {
			status = StatusNotFound
		}

		resultMap[ref] = LookupResult{
			ReferenceCode: ref,
			Status:        status,
			Subscriber: SubscriberInfo{
				AccountID: acc,
				Username:  user,
				Name:      name,
				Address:   addr,
				Phone:     phone,
			},
			Session: SessionInfo{
				CallingStationID: mac,
			},
		}
	}

	results := make([]LookupResult, len(lookups))
	for i, l := range lookups {
		if res, ok := resultMap[l.ReferenceCode]; ok {
			results[i] = res
		} else {
			results[i] = LookupResult{
				ReferenceCode: l.ReferenceCode,
				Status:        StatusNotFound,
			}
		}
	}

	return results, nil
}

func extractArray(root any, path string) []any {
	if path == "" {
		if arr, ok := root.([]any); ok {
			return arr
		}
		if m, ok := root.(map[string]any); ok {
			for _, v := range m {
				if arr, ok := v.([]any); ok {
					return arr
				}
			}
		}
		return nil
	}

	parts := strings.Split(path, ".")
	current := root
	for _, p := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[p]
	}

	if arr, ok := current.([]any); ok {
		return arr
	}
	return nil
}

func getString(m map[string]any, path string) string {
	if path == "" {
		return ""
	}
	parts := strings.Split(path, ".")
	var current any = m
	for _, p := range parts {
		cm, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = cm[p]
	}
	if current == nil {
		return ""
	}
	return fmt.Sprintf("%v", current)
}

func firstNonEmpty(items ...string) string {
	for _, it := range items {
		if strings.TrimSpace(it) != "" {
			return it
		}
	}
	return ""
}
