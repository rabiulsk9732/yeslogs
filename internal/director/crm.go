package director

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	universalcrm "github.com/natflow/natflow-dataplane/internal/director/crm"
)

const (
	crmSchemaVersion   = "yeslogs.crm.lookup.v1"
	crmDefaultTimeout  = 5000
	crmDefaultBatch    = 500
	crmMaxBatch        = 1000
	crmMaxResponseBody = 8 << 20
	crmCacheMaxEntries = 50000
	crmSecretPrefix    = "enc:v1:"
)

// CRMConnectorSettings configures one ISP's outbound CRM/RADIUS session
// resolver. APIKey is held in memory, encrypted before DB persistence, and
// omitted from every read response.
type CRMConnectorSettings struct {
	ISPID     uint32 `json:"ispId"`
	Enabled   bool   `json:"enabled"`
	Type      string `json:"type,omitempty"` // yeslogs_v1 | routeros
	Endpoint  string `json:"endpoint"`
	Username  string `json:"username,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	TimeoutMs int    `json:"timeoutMs"`
	BatchSize int    `json:"batchSize"`
}

// crmEnrichmentSummary accompanies search results and report metadata. Counts
// are row counts (not HTTP request counts), which is what the operator sees.
type crmEnrichmentSummary struct {
	Enabled       bool   `json:"enabled"`
	Status        string `json:"status"`
	Rows          int    `json:"rows"`
	UniqueLookups int    `json:"uniqueLookups"`
	Matched       int    `json:"matched"`
	NotFound      int    `json:"notFound"`
	Ambiguous     int    `json:"ambiguous"`
	Failed        int    `json:"failed"`
	Cached        int    `json:"cached"`
	ElapsedMs     int64  `json:"elapsedMs"`
}

type crmBatchRequest struct {
	SchemaVersion string      `json:"schemaVersion"`
	RequestID     string      `json:"requestId"`
	Lookups       []crmLookup `json:"lookups"`
}

type crmLookup struct {
	ReferenceCode string `json:"referenceCode"`
	LocalIP       string `json:"localIp"`
	EventTime     string `json:"eventTime"`
	DeviceID      uint32 `json:"deviceId"`
	NASIdentifier string `json:"nasIdentifier,omitempty"`
	NASIPAddress  string `json:"nasIpAddress,omitempty"`
}

type crmBatchResponse struct {
	SchemaVersion string      `json:"schemaVersion"`
	RequestID     string      `json:"requestId"`
	Results       []crmResult `json:"results"`
}

type crmResult struct {
	ReferenceCode string        `json:"referenceCode"`
	Status        string        `json:"status"`
	Subscriber    crmSubscriber `json:"subscriber"`
	Session       crmSession    `json:"session"`
}

type crmSubscriber struct {
	AccountID string `json:"accountId"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Address   string `json:"address"`
	Phone     string `json:"phone"`
}

type crmSession struct {
	AcctSessionID    string `json:"acctSessionId"`
	CallingStationID string `json:"callingStationId,omitempty"`
}

type crmCacheEntry struct {
	result  crmResult
	expires time.Time
}

type crmPendingLookup struct {
	key     string
	lookup  crmLookup
	rowIdxs []int
}

type crmDeviceIdentity struct {
	name       string
	exporterIP string
}

// sanitizeCRMConnector normalizes operator input and rejects endpoint shapes
// that could leak the Bearer token through URL credentials or redirects. Plain
// HTTP is accepted only on loopback, which keeps local contract tests possible
// without permitting production PII/API keys over clear text.
func sanitizeCRMConnector(v CRMConnectorSettings) (CRMConnectorSettings, error) {
	v.Type = strings.ToLower(strings.TrimSpace(v.Type))
	if v.Type == "" {
		v.Type = "yeslogs_v1"
	}
	if v.Type != "yeslogs_v1" && v.Type != "routeros" {
		return v, errors.New("unsupported CRM connector type")
	}
	v.Endpoint = strings.TrimSpace(v.Endpoint)
	v.Username = strings.TrimSpace(v.Username)
	v.APIKey = strings.TrimSpace(v.APIKey)
	if v.TimeoutMs < 500 || v.TimeoutMs > 30000 {
		v.TimeoutMs = crmDefaultTimeout
	}
	if v.BatchSize < 1 || v.BatchSize > crmMaxBatch {
		v.BatchSize = crmDefaultBatch
	}
	if len(v.Endpoint) > 2048 {
		return v, errors.New("CRM endpoint is too long")
	}
	if len(v.APIKey) > 4096 {
		return v, errors.New("CRM API key is too long")
	}
	if v.Endpoint == "" {
		if v.Enabled {
			return v, errors.New("CRM endpoint is required when enabled")
		}
		return v, nil
	}
	u, err := url.Parse(v.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return v, errors.New("CRM endpoint must be a valid http(s) URL")
	}
	if u.User != nil || u.Fragment != "" {
		return v, errors.New("CRM endpoint cannot contain credentials or a fragment")
	}
	if u.Scheme == "http" {
		host := strings.Trim(strings.ToLower(u.Hostname()), "[]")
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return v, errors.New("CRM endpoint must use HTTPS")
		}
	}
	if v.Type == "routeros" {
		if u.Scheme != "https" {
			return v, errors.New("RouterOS endpoint must use HTTPS")
		}
		if v.Username == "" {
			return v, errors.New("RouterOS username is required")
		}
	}
	return v, nil
}

func (s *Server) crmConnector(ispID uint32) (CRMConnectorSettings, bool) {
	if ispID == 0 {
		return CRMConnectorSettings{}, false
	}
	for _, c := range s.CurrentSettings().CRMConnectors {
		if c.ISPID == ispID {
			clean, err := sanitizeCRMConnector(c)
			if err != nil || !clean.Enabled || clean.APIKey == "" {
				return CRMConnectorSettings{}, false
			}
			return clean, true
		}
	}
	return CRMConnectorSettings{}, false
}

func recordInstant(r natRecord) (time.Time, bool) {
	if !r.At.IsZero() {
		return r.At.UTC().Truncate(time.Second), true
	}
	if t := parseTime(r.Time); !t.IsZero() {
		return t.UTC().Truncate(time.Second), true
	}
	return time.Time{}, false
}

// Resolve the internal endpoint only when translation direction is established.
// Empty Translation is retained for internal legacy callers; all search readers
// populate an explicit status, including unknown historical destination tuples.
func crmLocalIP(r natRecord) string {
	switch r.Translation {
	case "destination":
		return r.PostDstIP
	case "source", "":
		return r.PrivIP
	default:
		return ""
	}
}

func crmLookupHash(ispID uint32, endpoint string, r natRecord, at time.Time) string {
	raw := strings.Join([]string{
		crmSchemaVersion, endpoint, strconv.FormatUint(uint64(ispID), 10),
		strconv.FormatUint(uint64(r.DevID), 10), r.ExporterIP, crmLocalIP(r), at.UTC().Format(time.RFC3339),
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func crmReference(ispID uint32, r natRecord, at time.Time) string {
	raw := strings.Join([]string{
		crmSchemaVersion, strconv.FormatUint(uint64(ispID), 10),
		strconv.FormatUint(uint64(r.DevID), 10), r.ExporterIP, crmLocalIP(r), at.UTC().Format(time.RFC3339),
	}, "|")
	sum := sha256.Sum256([]byte(raw))
	// 96 hash bits keep the correlation code compact while avoiding birthday
	// collisions even across multi-billion-row daily archives.
	return "YL-" + at.In(istLoc).Format("20060102") + "-" + strings.ToUpper(hex.EncodeToString(sum[:12]))
}

func newCRMRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "YLREQ-" + strings.ToUpper(hex.EncodeToString(b[:]))
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return "YLREQ-" + strings.ToUpper(hex.EncodeToString(sum[:12]))
}

func (s *Server) crmCacheGet(key string) (crmResult, bool) {
	now := time.Now()
	s.crmMu.Lock()
	defer s.crmMu.Unlock()
	e, ok := s.crmCache[key]
	if !ok {
		return crmResult{}, false
	}
	if now.After(e.expires) {
		delete(s.crmCache, key)
		return crmResult{}, false
	}
	return e.result, true
}

func (s *Server) crmCachePut(key string, result crmResult) {
	ttl := 15 * time.Minute
	if result.Status != "matched" {
		ttl = 2 * time.Minute
	}
	now := time.Now()
	s.crmMu.Lock()
	defer s.crmMu.Unlock()
	if s.crmCache == nil {
		s.crmCache = map[string]crmCacheEntry{}
	}
	if len(s.crmCache) >= crmCacheMaxEntries {
		for k, v := range s.crmCache {
			if now.After(v.expires) {
				delete(s.crmCache, k)
			}
		}
		// Keep the cache strictly bounded even when every entry is still live.
		for k := range s.crmCache {
			if len(s.crmCache) < crmCacheMaxEntries*9/10 {
				break
			}
			delete(s.crmCache, k)
		}
	}
	s.crmCache[key] = crmCacheEntry{result: result, expires: now.Add(ttl)}
}

func (s *Server) resetCRMCache() {
	s.crmMu.Lock()
	s.crmCache = map[string]crmCacheEntry{}
	s.crmMu.Unlock()
}

func trimCRMText(v string, max int) string {
	v = strings.TrimSpace(v)
	r := []rune(v)
	if len(r) > max {
		return string(r[:max])
	}
	return v
}

func normalizeCRMResult(v crmResult) crmResult {
	v.ReferenceCode = trimCRMText(v.ReferenceCode, 80)
	v.Status = strings.ToLower(strings.TrimSpace(v.Status))
	switch v.Status {
	case "matched", "not_found", "ambiguous", "error":
	default:
		v.Status = "error"
	}
	v.Subscriber.AccountID = trimCRMText(v.Subscriber.AccountID, 190)
	v.Subscriber.Username = trimCRMText(v.Subscriber.Username, 190)
	v.Subscriber.Name = trimCRMText(v.Subscriber.Name, 256)
	v.Subscriber.Address = trimCRMText(v.Subscriber.Address, 1024)
	v.Subscriber.Phone = trimCRMText(v.Subscriber.Phone, 64)
	v.Session.AcctSessionID = trimCRMText(v.Session.AcctSessionID, 190)
	v.Session.CallingStationID = trimCRMText(v.Session.CallingStationID, 64)
	return v
}

func applyCRMResult(r *natRecord, v crmResult) {
	r.CRMStatus = v.Status
	r.CRMUsername = v.Subscriber.Username
	r.CRMName = v.Subscriber.Name
	r.CRMAddress = v.Subscriber.Address
	r.CRMPhone = v.Subscriber.Phone
	r.CRMAccountID = v.Subscriber.AccountID
	r.CRMSessionID = v.Session.AcctSessionID
	r.CRMMAC = v.Session.CallingStationID
}

func addCRMCount(sum *crmEnrichmentSummary, status string, rows int) {
	switch status {
	case "matched":
		sum.Matched += rows
	case "not_found":
		sum.NotFound += rows
	case "ambiguous":
		sum.Ambiguous += rows
	default:
		sum.Failed += rows
	}
}

// enrichCRMRows resolves private source IP + event timestamp + NAS identity
// against the selected ISP's remote CRM. It is fail-open for log availability:
// transport/contract errors mark only the enrichment as unavailable and never
// discard or mutate the underlying IPDR fields.
func (s *Server) enrichCRMRows(ctx context.Context, ispID uint32, rows []natRecord) crmEnrichmentSummary {
	started := time.Now()
	sum := crmEnrichmentSummary{Status: "disabled", Rows: len(rows)}
	cfg, enabled := s.crmConnector(ispID)
	if !enabled {
		return sum
	}
	sum.Enabled = true
	sum.Status = "complete"
	if len(rows) == 0 {
		return sum
	}

	devs, err := s.store.ListDevices(ctx, ispID)
	if err != nil {
		sum.Status, sum.Failed = "unavailable", len(rows)
		sum.ElapsedMs = time.Since(started).Milliseconds()
		return sum
	}

	pendingByKey := make(map[string]*crmPendingLookup)
	for i := range rows {
		at, ok := recordInstant(rows[i])
		localIP := crmLocalIP(rows[i])
		if localIP == "" {
			rows[i].CRMStatus = "insufficient_nat_data"
			sum.Failed++
			continue
		}
		if !ok || net.ParseIP(localIP) == nil {
			rows[i].CRMStatus = "error"
			sum.Failed++
			continue
		}
		key := crmLookupHash(ispID, cfg.Endpoint, rows[i], at)
		ref := crmReference(ispID, rows[i], at)
		rows[i].CRMReference = ref
		if cached, ok := s.crmCacheGet(key); ok {
			applyCRMResult(&rows[i], cached)
			addCRMCount(&sum, cached.Status, 1)
			sum.Cached++
			continue
		}
		if p, ok := pendingByKey[key]; ok {
			p.rowIdxs = append(p.rowIdxs, i)
			continue
		}
		dev, identified := resolveRecordDevice(devs, ispID, rows[i])
		if !identified {
			rows[i].CRMStatus = "ambiguous"
			sum.Ambiguous++
			continue
		}
		pendingByKey[key] = &crmPendingLookup{
			key: key,
			lookup: crmLookup{
				ReferenceCode: ref,
				LocalIP:       localIP,
				EventTime:     at.Format(time.RFC3339),
				DeviceID:      rows[i].DevID,
				NASIdentifier: dev.Name,
				NASIPAddress:  dev.ExporterIP,
			},
			rowIdxs: []int{i},
		}
	}

	pending := make([]*crmPendingLookup, 0, len(pendingByKey))
	for _, p := range pendingByKey {
		pending = append(pending, p)
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].lookup.ReferenceCode < pending[j].lookup.ReferenceCode
	})
	sum.UniqueLookups = len(pending)
	if len(pending) == 0 {
		if sum.Failed > 0 {
			sum.Status = "partial"
		}
		sum.ElapsedMs = time.Since(started).Milliseconds()
		return sum
	}

	results := make(map[string]crmResult, len(pending))
	var resultMu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan []*crmPendingLookup)
	workers := (len(pending) + cfg.BatchSize - 1) / cfg.BatchSize
	if workers > 4 {
		workers = 4
	}
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for batch := range jobs {
				lookups := make([]crmLookup, len(batch))
				for i, p := range batch {
					lookups[i] = p.lookup
				}
				got, err := s.sendCRMBatch(ctx, cfg, lookups)
				if err != nil {
					s.log.Warn("CRM enrichment request failed", "isp_id", ispID, "lookups", len(batch), "error", err)
					continue
				}
				resultMu.Lock()
				for ref, v := range got {
					results[ref] = v
				}
				resultMu.Unlock()
			}
		}()
	}
enqueue:
	for first := 0; first < len(pending); first += cfg.BatchSize {
		last := first + cfg.BatchSize
		if last > len(pending) {
			last = len(pending)
		}
		batch := append([]*crmPendingLookup(nil), pending[first:last]...)
		select {
		case jobs <- batch:
		case <-ctx.Done():
			break enqueue
		}
	}
	close(jobs)
	wg.Wait()

	for _, p := range pending {
		v, ok := results[p.lookup.ReferenceCode]
		if !ok {
			v = crmResult{ReferenceCode: p.lookup.ReferenceCode, Status: "error"}
		}
		v = normalizeCRMResult(v)
		for _, idx := range p.rowIdxs {
			applyCRMResult(&rows[idx], v)
		}
		addCRMCount(&sum, v.Status, len(p.rowIdxs))
		if v.Status == "matched" || v.Status == "not_found" || v.Status == "ambiguous" {
			s.crmCachePut(p.key, v)
		}
	}
	switch {
	case sum.Failed == len(rows):
		sum.Status = "unavailable"
	case sum.Failed > 0:
		sum.Status = "partial"
	}
	sum.ElapsedMs = time.Since(started).Milliseconds()
	return sum
}

func (s *Server) sendCRMBatch(ctx context.Context, cfg CRMConnectorSettings, lookups []crmLookup) (map[string]crmResult, error) {
	if cfg.Type == "routeros" {
		callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
		defer cancel()
		connector, err := universalcrm.NewRouterOSConnector(universalcrm.RouterOSConfig{
			Endpoint: cfg.Endpoint, Username: cfg.Username, Password: cfg.APIKey,
			Timeout: time.Duration(cfg.TimeoutMs) * time.Millisecond, HTTPClient: s.crmHTTP,
		})
		if err != nil {
			return nil, err
		}
		input := make([]universalcrm.LookupRequest, len(lookups))
		for i, q := range lookups {
			at, err := time.Parse(time.RFC3339, q.EventTime)
			if err != nil {
				return nil, fmt.Errorf("invalid lookup event time: %w", err)
			}
			input[i] = universalcrm.LookupRequest{ReferenceCode: q.ReferenceCode, LocalIP: q.LocalIP, EventTime: at, DeviceID: q.DeviceID, NASIdentifier: q.NASIdentifier, NASIPAddress: q.NASIPAddress}
		}
		got, err := connector.Lookup(callCtx, input)
		if err != nil {
			return nil, err
		}
		out := make(map[string]crmResult, len(got))
		for _, v := range got {
			out[v.ReferenceCode] = crmResult{
				ReferenceCode: v.ReferenceCode, Status: string(v.Status),
				Subscriber: crmSubscriber{AccountID: v.Subscriber.AccountID, Username: v.Subscriber.Username, Name: v.Subscriber.Name, Address: v.Subscriber.Address, Phone: v.Subscriber.Phone},
				Session:    crmSession{AcctSessionID: v.Session.AcctSessionID, CallingStationID: v.Session.CallingStationID},
			}
		}
		return out, nil
	}
	requestID := newCRMRequestID()
	payload, err := json.Marshal(crmBatchRequest{
		SchemaVersion: crmSchemaVersion,
		RequestID:     requestID,
		Lookups:       lookups,
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "YesLogs-Director/1")
	req.Header.Set("X-YesLogs-Schema", crmSchemaVersion)
	req.Header.Set("Idempotency-Key", requestID)

	resp, err := s.crmHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, crmMaxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > crmMaxResponseBody {
		return nil, errors.New("response exceeds 8 MiB")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var decoded crmBatchResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if decoded.RequestID != "" && decoded.RequestID != requestID {
		return nil, errors.New("response requestId mismatch")
	}
	if decoded.SchemaVersion != "" && decoded.SchemaVersion != crmSchemaVersion {
		return nil, errors.New("response schemaVersion mismatch")
	}
	expected := make(map[string]struct{}, len(lookups))
	for _, q := range lookups {
		expected[q.ReferenceCode] = struct{}{}
	}
	out := make(map[string]crmResult, len(decoded.Results))
	for _, raw := range decoded.Results {
		v := normalizeCRMResult(raw)
		if _, ok := expected[v.ReferenceCode]; !ok {
			return nil, errors.New("response contains an unknown referenceCode")
		}
		if _, dup := out[v.ReferenceCode]; dup {
			return nil, errors.New("response contains a duplicate referenceCode")
		}
		out[v.ReferenceCode] = v
	}
	return out, nil
}

func (s *Server) crmCipher() (cipher.AEAD, error) {
	material := make([]byte, 0, len(s.sessionKey)+24)
	material = append(material, "yeslogs-crm-secret-v1:"...)
	material = append(material, s.sessionKey...)
	key := sha256.Sum256(material)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Server) sealCRMSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	aead, err := s.crmCipher()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, nonce, []byte(plain), []byte(crmSchemaVersion))
	buf := append(nonce, sealed...)
	return crmSecretPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Server) openCRMSecret(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	// Backward-compatible one-time migration path for an early/manual plaintext
	// setting: it is accepted in memory and encrypted on the next save.
	if !strings.HasPrefix(value, crmSecretPrefix) {
		return value, nil
	}
	aead, err := s.crmCipher()
	if err != nil {
		return "", err
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, crmSecretPrefix))
	if err != nil || len(raw) < aead.NonceSize() {
		return "", errors.New("invalid encrypted CRM secret")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(crmSchemaVersion))
	if err != nil {
		return "", errors.New("could not decrypt CRM secret")
	}
	return string(plain), nil
}

func (s *Server) marshalCRMConnectors(connectors []CRMConnectorSettings) ([]byte, error) {
	persisted := append([]CRMConnectorSettings(nil), connectors...)
	for i := range persisted {
		sealed, err := s.sealCRMSecret(persisted[i].APIKey)
		if err != nil {
			return nil, err
		}
		persisted[i].APIKey = sealed
	}
	return json.Marshal(persisted)
}

func (s *Server) unmarshalCRMConnectors(raw string) ([]CRMConnectorSettings, error) {
	var connectors []CRMConnectorSettings
	if err := json.Unmarshal([]byte(raw), &connectors); err != nil {
		return nil, err
	}
	seen := map[uint32]bool{}
	for i := range connectors {
		if connectors[i].ISPID == 0 || seen[connectors[i].ISPID] {
			return nil, errors.New("invalid duplicate CRM connector")
		}
		seen[connectors[i].ISPID] = true
		key, err := s.openCRMSecret(connectors[i].APIKey)
		if err != nil {
			return nil, err
		}
		connectors[i].APIKey = key
		v, err := sanitizeCRMConnector(connectors[i])
		if err != nil {
			return nil, err
		}
		if v.Enabled && v.APIKey == "" {
			return nil, errors.New("enabled CRM connector has no API key")
		}
		connectors[i] = v
	}
	sort.Slice(connectors, func(i, j int) bool { return connectors[i].ISPID < connectors[j].ISPID })
	return connectors, nil
}

func (s *Server) handleTestCRM(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	var body struct {
		ISPID     uint32 `json:"ispId"`
		LocalIP   string `json:"localIp"`
		EventTime string `json:"eventTime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	scope, err := id.scopeISP(body.ISPID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	cfg, enabled := s.crmConnector(scope)
	if !enabled {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "CRM connector not enabled or not configured for this ISP"})
		return
	}
	localIP := strings.TrimSpace(body.LocalIP)
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	evTime := time.Now().UTC()
	if body.EventTime != "" {
		if t := parseTime(body.EventTime); !t.IsZero() {
			evTime = t.UTC()
		}
	}
	refCode := "YL-TEST-" + newCRMRequestID()
	lookup := crmLookup{
		ReferenceCode: refCode,
		LocalIP:       localIP,
		EventTime:     evTime.Format(time.RFC3339),
	}
	resMap, err := s.sendCRMBatch(r.Context(), cfg, []crmLookup{lookup})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	res, ok := resMap[lookup.ReferenceCode]
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "not_found",
			"error":  "No result returned for test lookup",
		})
		return
	}
	res = normalizeCRMResult(res)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     res.Status,
		"subscriber": res.Subscriber,
		"session":    res.Session,
	})
}
