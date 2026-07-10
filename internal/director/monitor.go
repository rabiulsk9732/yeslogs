package director

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// Notifier delivers one alert (subject + plain-text body) to the configured
// recipients. The host (natlog) wires this to SMTP; nil means alerting is off.
// It reads the live recipient/credential settings itself, so the monitor stays
// transport-agnostic.
type Notifier func(ctx context.Context, subject, body string) error

// SetNotifier installs the alert transport.
func (s *Server) SetNotifier(n Notifier) {
	s.notifyMu.Lock()
	s.notifier = n
	s.notifyMu.Unlock()
}

func (s *Server) getNotifier() Notifier {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notifier
}

// devHealthState is the per-device liveness state retained across checks so we
// only email on transitions (and on the RemindHours cadence while still down).
type devHealthState struct {
	lastFlow    time.Time
	status      string // "init" | "healthy" | "silent"
	lastAlertAt time.Time
}

// DeviceHealth is the per-device liveness surfaced to the console.
type DeviceHealth struct {
	LastSeen time.Time `json:"lastSeen"` // zero = no flows in the lookback window
	Status   string    `json:"status"`   // online | silent | nodata
	AgoSecs  int64     `json:"agoSecs"`  // seconds since last flow (0 when nodata)
}

const healthLookbackDays = 3

func exporterKey(ispID uint32, ip string) string { return fmt.Sprintf("%d|%s", ispID, ip) }

func notifSilenceMins(n NotificationSettings) int {
	if n.SilenceMins < 1 {
		return 15
	}
	return n.SilenceMins
}

// deviceHealthMap computes per-device status (keyed by device ID) for UI badges.
// It uses the same silence threshold as the alerting monitor so the badge and
// the emails agree. Returns an empty map when ClickHouse is unavailable.
func (s *Server) deviceHealthMap(ctx context.Context, devs []store.Device) map[int64]DeviceHealth {
	out := make(map[int64]DeviceHealth, len(devs))
	if s.flows == nil || len(devs) == 0 {
		return out
	}
	qctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	seen, err := s.flows.LastSeenByExporter(qctx, healthLookbackDays)
	if err != nil {
		s.log.Warn("device health: last-seen query failed", "error", err)
		return out
	}
	silence := time.Duration(notifSilenceMins(s.CurrentSettings().Notifications)) * time.Minute
	now := time.Now()
	for _, d := range devs {
		ls := seen[exporterKey(d.ISPID, d.ExporterIP)]
		h := DeviceHealth{LastSeen: ls}
		switch {
		case ls.IsZero():
			h.Status = "nodata"
		case now.Sub(ls) > silence:
			h.Status = "silent"
			h.AgoSecs = int64(now.Sub(ls).Seconds())
		default:
			h.Status = "online"
			h.AgoSecs = int64(now.Sub(ls).Seconds())
		}
		out[d.ID] = h
	}
	return out
}

// RunDeviceMonitor runs the device-liveness alert loop until ctx is cancelled.
// It is a no-op each tick unless notifications are enabled and a notifier + flow
// reader are wired. Safe to start unconditionally.
func (s *Server) RunDeviceMonitor(ctx context.Context) {
	first := time.NewTimer(90 * time.Second) // let flows arrive after a restart before judging
	defer first.Stop()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			s.runHealthCheck(ctx)
		case <-tick.C:
			s.runHealthCheck(ctx)
		}
	}
}

type pendingAlert struct {
	d         store.Device
	st        devHealthState
	recovered bool
}

func (s *Server) runHealthCheck(ctx context.Context) {
	n := s.CurrentSettings().Notifications
	if !n.Enabled || s.flows == nil {
		return
	}
	notifier := s.getNotifier()
	if notifier == nil {
		return
	}
	// Queries get their own bounded context, cancelled before any email is sent
	// so a slow ClickHouse query can't consume the email-send budget.
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	devs, err := s.store.ListDevices(qctx, 0)
	if err != nil {
		cancel()
		s.log.Error("device monitor: list devices failed", "error", err)
		return
	}
	seen, err := s.flows.LastSeenByExporter(qctx, healthLookbackDays)
	cancel()
	if err != nil {
		s.log.Error("device monitor: last-seen query failed", "error", err)
		return
	}

	silence := time.Duration(notifSilenceMins(n)) * time.Minute
	remind := time.Duration(n.RemindHours) * time.Hour
	toSend := s.evalTransitions(devs, seen, silence, remind, time.Now())

	// Send OUTSIDE the state lock, each with its own fresh timeout, so a slow or
	// large alert batch can neither block state updates nor starve later sends.
	for _, p := range toSend {
		sctx, sc := context.WithTimeout(ctx, 30*time.Second)
		s.sendDeviceAlert(sctx, notifier, p.d, p.st, p.recovered)
		sc()
	}
}

// evalTransitions mutates the per-device liveness state under healthMu and
// returns the alerts to email. No I/O happens while the lock is held.
func (s *Server) evalTransitions(devs []store.Device, seen map[string]time.Time, silence, remind time.Duration, now time.Time) []pendingAlert {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health == nil {
		s.health = map[string]*devHealthState{}
	}
	live := make(map[string]bool, len(devs))
	var toSend []pendingAlert
	for _, d := range devs {
		if !d.Enabled {
			continue
		}
		key := exporterKey(d.ISPID, d.ExporterIP)
		live[key] = true
		st := s.health[key]
		if st == nil {
			st = &devHealthState{status: "init"}
			s.health[key] = st
		}
		if ls := seen[key]; ls.After(st.lastFlow) {
			st.lastFlow = ls // only ratchet upward; survives the device leaving the lookback window
		}
		if st.lastFlow.IsZero() {
			continue // never seen any flow — don't alert (likely not configured yet)
		}
		down := now.Sub(st.lastFlow) > silence
		switch {
		case down && st.status != "silent":
			st.status = "silent"
			st.lastAlertAt = now
			toSend = append(toSend, pendingAlert{d, *st, false})
		case down && st.status == "silent" && remind > 0 && now.Sub(st.lastAlertAt) >= remind:
			st.lastAlertAt = now
			toSend = append(toSend, pendingAlert{d, *st, false})
		case !down && st.status == "silent":
			st.status = "healthy"
			toSend = append(toSend, pendingAlert{d, *st, true})
		case !down:
			st.status = "healthy"
		}
	}
	// Forget state for devices that were removed/disabled (avoids stale recovery).
	for k := range s.health {
		if !live[k] {
			delete(s.health, k)
		}
	}
	return toSend
}

func (s *Server) sendDeviceAlert(ctx context.Context, notifier Notifier, d store.Device, st devHealthState, recovered bool) {
	last := "never"
	if !st.lastFlow.IsZero() {
		last = st.lastFlow.In(istLoc).Format("2006-01-02 15:04:05 IST")
	}
	var subject, body string
	if recovered {
		subject = fmt.Sprintf("[YesLogs] RECOVERED: %s (%s) is sending flows again", d.Name, d.ExporterIP)
		body = fmt.Sprintf("Device %q (exporter %s) has resumed sending NetFlow/IPFIX.\n\nLast flow: %s\n\n— YesLogs Director", d.Name, d.ExporterIP, last)
	} else {
		mins := int(time.Since(st.lastFlow).Minutes())
		subject = fmt.Sprintf("[YesLogs] DEVICE DOWN: %s (%s) — no flows for %dm", d.Name, d.ExporterIP, mins)
		body = fmt.Sprintf("Device %q (exporter %s) has stopped sending NetFlow/IPFIX.\n\nLast flow seen: %s (%d minutes ago)\n\nCheck the exporter's traffic-flow/export config and the link to the collector.\n\n— YesLogs Director", d.Name, d.ExporterIP, last, mins)
	}
	if err := notifier(ctx, subject, body); err != nil {
		s.log.Error("device alert email failed", "device", d.Name, "recovered", recovered, "error", err)
		return
	}
	s.log.Info("device alert sent", "device", d.Name, "exporter", d.ExporterIP, "recovered", recovered)
}

// handleTestNotification sends a test email using the saved SMTP settings
// (director only). Surfaces the SMTP error to the director to aid configuration.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
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
	notifier := s.getNotifier()
	if notifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "notifications not available in this build"})
		return
	}
	n := s.CurrentSettings().Notifications
	if n.SMTPHost == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "set the SMTP host and save before testing"})
		return
	}
	if n.Recipients == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "add at least one recipient and save before testing"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	body := fmt.Sprintf("This is a test alert from YesLogs Director, requested by %s.\n\nIf you received this, SMTP alerting is configured correctly.\n\n— YesLogs Director", id.Email)
	if err := notifier(ctx, "[YesLogs] Test alert", body); err != nil {
		s.log.Error("test notification failed", "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "send failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
