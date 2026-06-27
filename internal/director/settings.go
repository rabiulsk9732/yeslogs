package director

import (
	"context"
	"encoding/json"
	"net/http"
	"net/mail"
	"strings"

	"github.com/natflow/natflow-dataplane/internal/notify"
)

// Settings is the editable, DB-backed configuration surfaced in the console.
// YAML provides only the bootstrap defaults; the DB is the source of truth and
// changes are applied to the running dataplane via the applier callback.
type Settings struct {
	Dataplane     DataplaneSettings    `json:"dataplane"`
	SkipRules     SkipRuleSettings     `json:"skiprules"`
	Retention     RetentionSettings    `json:"retention"`
	S3            S3Settings           `json:"s3"`
	Notifications NotificationSettings `json:"notifications"`
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

// NotificationSettings configures device-liveness SMTP alerting. A device is
// flagged "silent" after SilenceMins with no flows; an email fires on the
// healthy→silent transition (and again every RemindHours while still silent),
// plus a recovery email when flows resume.
type NotificationSettings struct {
	Enabled      bool   `json:"enabled"`
	SMTPHost     string `json:"smtpHost"`
	SMTPPort     int    `json:"smtpPort"`     // 587 starttls | 465 tls | 25 none
	SMTPUser     string `json:"smtpUser"`     // empty = no AUTH
	SMTPPassword string `json:"smtpPassword"` // cleared on read; never returned to the client
	SMTPTLS      string `json:"smtpTls"`      // starttls | tls | none
	FromAddr     string `json:"fromAddr"`
	Recipients   string `json:"recipients"`  // comma/newline separated
	SilenceMins  int    `json:"silenceMins"` // mark silent after N min no flows
	RemindHours  int    `json:"remindHours"` // re-remind cadence while down (0 = once)
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
	if v, ok := raw["notifications"]; ok {
		_ = json.Unmarshal([]byte(v), &s.settings.Notifications)
		s.settings.Notifications = sanitizeNotifications(s.settings.Notifications)
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
	cur.S3.SecretKey = ""               // never expose the S3 secret
	cur.Notifications.SMTPPassword = "" // never expose the SMTP password
	out := map[string]any{
		"settings":      cur,
		"secretSet":     s.CurrentSettings().S3.SecretKey != "",
		"notifyPassSet": s.CurrentSettings().Notifications.SMTPPassword != "",
	}
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
		nf  NotificationSettings
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
	case "notifications":
		derr = dec.Decode(&nf)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown section"})
		return
	}
	if derr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// Fail fast on malformed recipient addresses so the operator finds out at save
	// time, not when a device-down alert silently fails to deliver.
	if section == "notifications" {
		for _, rc := range notify.SplitRecipients(nf.Recipients) {
			if _, perr := mail.ParseAddress(rc); perr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid recipient address: " + rc})
				return
			}
		}
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
	case "notifications":
		// Blank = keep the stored password (the UI sends empty when unchanged and
		// never echoes the value, so an exact-mask collision can't occur here).
		if strings.TrimSpace(nf.SMTPPassword) == "" {
			nf.SMTPPassword = s.settings.Notifications.SMTPPassword
		}
		nf = sanitizeNotifications(nf)
		s.settings.Notifications = nf
		raw, _ = json.Marshal(nf)
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

func sanitizeNotifications(n NotificationSettings) NotificationSettings {
	n.SMTPHost = strings.TrimSpace(n.SMTPHost)
	n.SMTPUser = strings.TrimSpace(n.SMTPUser)
	n.FromAddr = strings.TrimSpace(n.FromAddr)
	if n.SMTPPort < 1 || n.SMTPPort > 65535 {
		n.SMTPPort = 587
	}
	switch n.SMTPTLS {
	case "starttls", "tls", "none":
	default:
		n.SMTPTLS = "starttls"
	}
	if n.SilenceMins < 1 {
		n.SilenceMins = 15
	}
	if n.SilenceMins > 1440 {
		n.SilenceMins = 1440
	}
	if n.RemindHours < 0 {
		n.RemindHours = 0
	}
	if n.RemindHours > 168 {
		n.RemindHours = 168
	}
	return n
}
