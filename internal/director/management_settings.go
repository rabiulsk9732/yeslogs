package director

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"
)

// Settings stay on the collector, where the runtime applier lives. The gateway
// supplies snapshot preconditions and rejects invalid forms before forwarding.
func settingsVersions(body []byte, persisted map[string]string) (map[string]string, error) {
	var response struct {
		Settings map[string]any `json:"settings"`
	}
	if e := json.Unmarshal(body, &response); e != nil {
		return nil, e
	}
	out := map[string]string{}
	digest := func(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
	for k, v := range response.Settings {
		if k == "crmConnectors" {
			if items, ok := v.([]any); ok {
				for _, item := range items {
					m, ok := item.(map[string]any)
					if ok {
						out[fmt.Sprintf("crm:%v", m["ispId"])] = digest(m)
					}
				}
			}
		} else {
			out[k] = digest(v)
		}
	}
	out["crm:new"] = digest(map[string]any{})
	for key, v := range out {
		section := strings.Split(key, ":")[0]
		out[key] = digest([]string{v, persisted[section]})
	}
	return out, nil
}
func (s *Server) decorateSettingsResponse(resp *http.Response) error {
	if resp.Request.Method != "GET" || resp.Request.URL.Path != "/api/v1/settings" || resp.StatusCode != 200 {
		return nil
	}
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		zipped, err := gzip.NewReader(resp.Body)
		if err != nil {
			return err
		}
		defer zipped.Close()
		reader = zipped
		resp.Header.Del("Content-Encoding")
	}
	body, e := io.ReadAll(io.LimitReader(reader, 1<<20))
	if e != nil {
		return e
	}
	persisted, e := s.store.GetSettings(resp.Request.Context())
	if e != nil {
		return e
	}
	versions, e := settingsVersions(body, persisted)
	if e != nil {
		return e
	}
	var data map[string]any
	if e = json.Unmarshal(body, &data); e != nil {
		return e
	}
	data["versions"] = versions
	body, e = json.Marshal(data)
	if e != nil {
		return e
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return nil
}
func (s *Server) checkRuntimeSettings(w http.ResponseWriter, r *http.Request, upstream string) bool {
	id, ok := s.authJSON(w, r)
	if !ok {
		return false
	}
	if !id.isDirector() {
		writeJSON(w, 403, map[string]string{"error": "forbidden"})
		return false
	}
	if !s.csrfOK(w, r, id) {
		return false
	}
	body, e := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid settings form"})
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var b map[string]any
	if e = json.Unmarshal(body, &b); e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid settings form"})
		return false
	}
	section := strings.TrimPrefix(r.URL.Path, "/api/v1/settings/")
	fields := validateSettingsForm(section, b)
	if userFields(w, fields) {
		return false
	}
	version := r.Header.Get("X-Settings-Version")
	if version == "" {
		writeJSON(w, 428, map[string]string{"error": "Refresh the console and reopen settings before saving."})
		return false
	}
	req, e := http.NewRequestWithContext(r.Context(), "GET", upstream+"/api/v1/settings", nil)
	if e != nil {
		s.jsonErr(w, e)
		return false
	}
	req.Header.Set("Cookie", r.Header.Get("Cookie"))
	client := &http.Client{Timeout: 10 * time.Second}
	resp, e := client.Do(req)
	if e != nil {
		writeJSON(w, 503, map[string]string{"error": "Cannot verify current runtime settings. No changes were sent."})
		return false
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil || resp.StatusCode != 200 {
		writeJSON(w, 503, map[string]string{"error": "Cannot verify current runtime settings. No changes were sent."})
		return false
	}
	persisted, e := s.store.GetSettings(r.Context())
	if e != nil {
		s.jsonErr(w, e)
		return false
	}
	versions, e := settingsVersions(raw, persisted)
	if e != nil {
		s.jsonErr(w, e)
		return false
	}
	key := section
	if section == "crm" {
		key = fmt.Sprintf("crm:%v", b["ispId"])
		if versions[key] == "" {
			key = "crm:new"
		}
	}
	if versions[key] == "" || versions[key] != version {
		writeJSON(w, 409, map[string]string{"error": "This section changed. Close and reopen it before saving."})
		return false
	}
	var current struct {
		SecretSet bool            `json:"secretSet"`
		CRMKeySet map[string]bool `json:"crmKeySet"`
	}
	if err := json.Unmarshal(raw, &current); err != nil {
		s.jsonErr(w, err)
		return false
	}
	fields = map[string]string{}
	if section == "s3" && b["enabled"] == true && strings.TrimSpace(fmt.Sprint(b["secretKey"])) == "" && !current.SecretSet {
		fields["secretKey"] = "Enter a secret key to enable S3."
	}
	if section == "crm" && b["enabled"] == true && strings.TrimSpace(fmt.Sprint(b["apiKey"])) == "" && !current.CRMKeySet[fmt.Sprint(b["ispId"])] {
		fields["apiKey"] = "Enter the connector API key."
	}
	if userFields(w, fields) {
		return false
	}
	// The collector retains stored secrets; it alone applies writes.
	return true
}
func validateSettingsForm(section string, b map[string]any) map[string]string {
	e := map[string]string{}
	num := func(k string, lo, hi float64) {
		v, ok := b[k].(float64)
		if !ok || v < lo || v > hi || v != float64(int64(v)) {
			e[k] = fmt.Sprintf("Enter a whole number from %.0f to %.0f.", lo, hi)
		}
	}
	boolean := func(k string) {
		if _, ok := b[k].(bool); !ok {
			e[k] = "Choose enabled or disabled."
		}
	}
	choice := func(k string, choices ...string) {
		v, ok := b[k].(string)
		if ok {
			for _, c := range choices {
				if v == c {
					return
				}
			}
		}
		e[k] = "Choose a valid option."
	}
	str := func(k string) string {
		v, ok := b[k].(string)
		if !ok {
			e[k] = "Enter a text value."
		}
		return strings.TrimSpace(v)
	}
	endpoint := func(k string, required bool, https bool) {
		v := str(k)
		if v == "" && !required {
			return
		}
		u, err := url.Parse(v)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") || (https && u.Scheme != "https" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
			e[k] = "Enter a valid endpoint URL without embedded credentials."
		}
	}
	switch section {
	case "dataplane":
		num("batchSize", 100, 1000000)
		num("flushIntervalMs", 50, 60000)
		num("writerWorkers", 1, 64)
		num("maxQueueRows", 1000, 100000000)
		choice("backpressureMode", "block", "drop_new", "drop_old")
		choice("unknownExporterMode", "allow", "observe", "reject")
	case "skiprules":
		for _, k := range []string{"skipDns", "skipPrivate", "skipZero"} {
			boolean(k)
		}
	case "retention":
		num("days", 1, 36500)
	case "s3":
		boolean("enabled")
		boolean("autoArchive")
		num("archiveAfterDays", 1, 36500)
		choice("exportFormat", "parquet", "csvgz")
		on, _ := b["enabled"].(bool)
		endpoint("endpoint", on, false)
		for _, k := range []string{"region", "bucket", "accessKey", "secretKey", "pathPrefix"} {
			v := str(k)
			if on && (k == "bucket" || k == "accessKey") && v == "" {
				e[k] = "Required when S3 is enabled."
			}
		}
		if b["autoArchive"] == true && !on {
			e["autoArchive"] = "Enable S3 before auto archive."
		}
	case "notifications":
		boolean("enabled")
		if _, ok := b["webhookEnabled"]; ok {
			boolean("webhookEnabled")
		}
		if _, ok := b["dailySummary"]; ok {
			boolean("dailySummary")
		}
		num("smtpPort", 1, 65535)
		num("silenceMins", 1, 1440)
		num("remindHours", 0, 168)
		if _, ok := b["dailyHourIst"]; ok {
			num("dailyHourIst", 0, 23)
		}
		choice("smtpTls", "starttls", "tls", "none")
		if _, ok := b["webhookType"]; ok {
			choice("webhookType", "generic", "slack", "discord", "telegram")
		}
		for _, k := range []string{"smtpHost", "smtpUser", "smtpPassword", "fromAddr", "recipients"} {
			str(k)
		}
		for _, k := range []string{"webhookUrl", "telegramChatId"} {
			if _, ok := b[k]; ok {
				str(k)
			}
		}
		if b["enabled"] == true {
			if str("smtpHost") == "" && b["webhookEnabled"] != true {
				e["smtpHost"] = "Enter an SMTP host or enable a webhook."
			}
			if str("smtpHost") != "" && str("recipients") == "" {
				e["recipients"] = "Enter at least one recipient."
			}
			if b["webhookEnabled"] == true {
				endpoint("webhookUrl", true, false)
			}
		}
		validEmail := func(v string) bool {
			a, err := mail.ParseAddress(v)
			return err == nil && a.Address == v && strings.Contains(v, ".")
		}
		if v := str("fromAddr"); v != "" && !validEmail(v) {
			e["fromAddr"] = "Enter a valid sender email address."
		}
		for _, rc := range strings.FieldsFunc(str("recipients"), func(r rune) bool { return r == ',' || r == '\n' }) {
			if !validEmail(strings.TrimSpace(rc)) {
				e["recipients"] = "Use valid email addresses separated by commas or newlines."
			}
		}
		if b["enabled"] == true && str("smtpHost") != "" && str("fromAddr") == "" && !validEmail(str("smtpUser")) {
			e["fromAddr"] = "Enter a sender email address."
		}

	case "crm":
		num("ispId", 1, 4294967295)
		boolean("enabled")
		num("timeoutMs", 500, 30000)
		num("batchSize", 1, 1000)
		on, _ := b["enabled"].(bool)
		endpoint("endpoint", on, true)
		str("apiKey")
	default:
		e["section"] = "Unknown settings section."
	}
	return e
}
