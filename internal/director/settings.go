package director

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Settings is the editable, DB-backed configuration surfaced in the console.
// YAML provides only the bootstrap defaults; the DB is the source of truth and
// changes are applied to the running dataplane via the applier callback.
type Settings struct {
	Dataplane DataplaneSettings `json:"dataplane"`
	SkipRules SkipRuleSettings  `json:"skiprules"`
	Retention RetentionSettings `json:"retention"`
	S3        S3Settings        `json:"s3"`
}

type DataplaneSettings struct {
	BatchSize           int    `json:"batchSize"`
	FlushIntervalMs     int    `json:"flushIntervalMs"`
	WriterWorkers       int    `json:"writerWorkers"`
	MaxQueueRows        int    `json:"maxQueueRows"`
	BackpressureMode    string `json:"backpressureMode"`    // block | drop_new | drop_old
	UnknownExporterMode string `json:"unknownExporterMode"` // allow | observe | reject
}

type SkipRuleSettings struct {
	SkipDNS     bool `json:"skipDns"`
	SkipPrivate bool `json:"skipPrivate"`
	SkipZero    bool `json:"skipZero"`
}

type RetentionSettings struct {
	Days int `json:"days"`
}

type S3Settings struct {
	Enabled          bool   `json:"enabled"`
	Endpoint         string `json:"endpoint"`
	Region           string `json:"region"`
	Bucket           string `json:"bucket"`
	AccessKey        string `json:"accessKey"`
	SecretKey        string `json:"secretKey"`
	PathPrefix       string `json:"pathPrefix"`
	ExportFormat     string `json:"exportFormat"`
	AutoArchive      bool   `json:"autoArchive"`      // move hot days to S3 automatically
	ArchiveAfterDays int    `json:"archiveAfterDays"` // hot window in days (e.g. 7 or 30)
}

const secretMask = "••••••••"

// InitSettings seeds the in-memory defaults then overlays any DB-persisted
// values. Call once at startup (before serving).
func (s *Server) InitSettings(ctx context.Context, defaults Settings) {
	s.settingsMu.Lock()
	s.settings = defaults
	s.settingsMu.Unlock()
	if raw, err := s.store.GetSettings(ctx); err == nil {
		s.applyPersisted(raw)
	}
}

func (s *Server) applyPersisted(raw map[string]string) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if v, ok := raw["dataplane"]; ok {
		_ = json.Unmarshal([]byte(v), &s.settings.Dataplane)
		s.settings.Dataplane = sanitizeDataplane(s.settings.Dataplane) // clamp legacy/tampered values
	}
	if v, ok := raw["skiprules"]; ok {
		_ = json.Unmarshal([]byte(v), &s.settings.SkipRules)
	}
	if v, ok := raw["retention"]; ok {
		_ = json.Unmarshal([]byte(v), &s.settings.Retention)
		if s.settings.Retention.Days < 1 {
			s.settings.Retention.Days = 180
		}
	}
	if v, ok := raw["s3"]; ok {
		_ = json.Unmarshal([]byte(v), &s.settings.S3)
	}
}

// SetApplier registers the callback that pushes settings into the dataplane.
func (s *Server) SetApplier(f func(Settings)) { s.applier = f }

// CurrentSettings returns a copy of the live settings.
func (s *Server) CurrentSettings() Settings {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	return s.settings
}

// Apply pushes the current settings into the dataplane. Serialized so concurrent
// saves apply in order and always converge to the latest persisted state.
func (s *Server) Apply() {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	if s.applier != nil {
		s.applier(s.CurrentSettings())
	}
}

// --- handlers (director only) ---

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	cur := s.CurrentSettings()
	cur.S3.SecretKey = "" // never expose the secret
	out := map[string]any{"settings": cur, "secretSet": s.CurrentSettings().S3.SecretKey != ""}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	if !id.isDirector() {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return
	}
	if !s.csrfOK(w, r, id) {
		return
	}
	section := r.PathValue("section")
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)

	// Decode OUTSIDE the lock (no network IO under the mutex).
	dec := json.NewDecoder(r.Body)
	var (
		dp  DataplaneSettings
		sk  SkipRuleSettings
		rt  RetentionSettings
		s3v S3Settings
	)
	var derr error
	switch section {
	case "dataplane":
		derr = dec.Decode(&dp)
	case "skiprules":
		derr = dec.Decode(&sk)
	case "retention":
		derr = dec.Decode(&rt)
	case "s3":
		derr = dec.Decode(&s3v)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown section"})
		return
	}
	if derr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}

	var raw []byte
	s.settingsMu.Lock()
	switch section {
	case "dataplane":
		s.settings.Dataplane = sanitizeDataplane(dp)
		raw, _ = json.Marshal(s.settings.Dataplane)
	case "skiprules":
		s.settings.SkipRules = sk
		raw, _ = json.Marshal(sk)
	case "retention":
		if rt.Days < 1 {
			rt.Days = 1
		}
		s.settings.Retention = rt
		raw, _ = json.Marshal(rt)
	case "s3":
		if strings.TrimSpace(s3v.SecretKey) == "" || s3v.SecretKey == secretMask {
			s3v.SecretKey = s.settings.S3.SecretKey // preserve existing secret
		}
		s.settings.S3 = s3v
		raw, _ = json.Marshal(s3v)
	}
	s.settingsMu.Unlock()

	if err := s.store.PutSetting(r.Context(), section, string(raw)); err != nil {
		s.log.Error("save setting", "section", section, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save"})
		return
	}
	s.Apply() // serialized; applies the latest persisted settings to the dataplane
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func sanitizeDataplane(v DataplaneSettings) DataplaneSettings {
	clamp := func(x, lo, hi, def int) int {
		if x < lo || x > hi {
			return def
		}
		return x
	}
	v.BatchSize = clamp(v.BatchSize, 100, 1000000, 5000)
	v.FlushIntervalMs = clamp(v.FlushIntervalMs, 50, 60000, 300)
	v.WriterWorkers = clamp(v.WriterWorkers, 1, 64, 2)
	v.MaxQueueRows = clamp(v.MaxQueueRows, 1000, 100000000, 200000)
	switch v.BackpressureMode {
	case "block", "drop_new", "drop_old":
	default:
		v.BackpressureMode = "block"
	}
	switch v.UnknownExporterMode {
	case "allow", "observe", "reject":
	default:
		v.UnknownExporterMode = "reject"
	}
	return v
}
