// IPDR compliance auditing.
//
// An ISP running CGNAT must be able to answer a lawful request of the form
// "at time T, which subscriber held public IP X, port P?". Answering it needs
// the exporter to log the NAT *translation* — the (private ip:port) <-> (public
// ip:port) pair — not merely that traffic flowed. An exporter can be perfectly
// healthy, green in every liveness view, and still emit nothing that can answer
// that question: it just exports traffic flows with the post-NAT fields unset.
//
// That failure is invisible to the liveness monitor (flows ARE arriving) and to
// the dashboard row counts (rows ARE being stored), which is exactly how a
// device can sit for days looking fine while logging nothing of legal value.
// This file closes that gap: it grades every device on whether the records it
// is producing can actually answer a lawful request, and alerts when one cannot.
//
// The grading is deliberately about capability, not volume: a device that
// translates a small fraction of its traffic is still able to answer requests
// about the flows it did translate. Volume is reported as an efficiency hint,
// never as a compliance verdict.
package director

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// ComplianceState grades one device's ability to answer a lawful request.
type ComplianceState string

const (
	// CompOK — translations with ports are being logged. Requests answerable.
	CompOK ComplianceState = "ok"
	// CompNoPort — the public IP is logged but the post-NAT port is not. Under
	// CGNAT one public IP is shared by many subscribers and the port is the
	// only discriminator, so these records cannot identify a subscriber.
	CompNoPort ComplianceState = "no_port"
	// CompPartialPort — some translations carry a port and some do not.
	CompPartialPort ComplianceState = "partial_port"
	// CompNoNATFields — flows arrive but the post-NAT address is unset (0.0.0.0):
	// the exporter is sending traffic flows, not NAT translation records.
	CompNoNATFields ComplianceState = "no_nat_fields"
	// CompNoTranslation — the post-NAT address is present but always equals the
	// source, i.e. nothing was actually translated (inbound/transit traffic).
	CompNoTranslation ComplianceState = "no_translation"
	// CompSilent — registered, but not one flow arrived in a whole day. Short
	// silences are the liveness monitor's job (RunDeviceMonitor); this grade
	// means the exporter is effectively gone.
	CompSilent ComplianceState = "silent"
	// CompDisabled — administratively disabled; excluded from grading.
	CompDisabled ComplianceState = "disabled"
)

// answerable reports whether records in this state can answer a lawful request.
func (c ComplianceState) answerable() bool { return c == CompOK || c == CompPartialPort }

// DeviceFlowStats holds one device's raw translation counters over the audit
// window, straight from ClickHouse. All counters cover the same row set.
type DeviceFlowStats struct {
	DeviceID   uint32
	Flows      uint64    // rows stored in the window
	Translated uint64    // post-NAT address set and different from src
	NoNATField uint64    // post-NAT address unset (0.0.0.0)
	SameAsSrc  uint64    // post-NAT address present but equal to src
	WithPort   uint64    // translated AND post-NAT port non-zero
	PrivateSrc uint64    // src in RFC1918/CGNAT space — a subscriber-side flow
	LastFlow   time.Time // newest flow_start in the window
}

// DeviceSignal is what an exporter is still telling us when the dataplane drops
// everything it sends. The hard-coded no-translation rule means a device that
// exports traffic rather than NAT stores zero rows, so every stored-row measure
// — liveness, silence alerts, this grade — would read it as dead without this.
type DeviceSignal struct {
	NoNATDropped uint64    // flows discarded for carrying no post-NAT address
	LastFlow     time.Time // when such a flow last arrived: proof of life
}

// SetDeviceSignals registers the dropped-flow evidence provider.
func (s *Server) SetDeviceSignals(f func() map[uint32]DeviceSignal) { s.signalsFn = f }

func (s *Server) deviceSignals() map[uint32]DeviceSignal {
	if s.signalsFn == nil {
		return map[uint32]DeviceSignal{}
	}
	return s.signalsFn()
}

// DeviceCompliance is one device's grade plus the evidence behind it.
type DeviceCompliance struct {
	StoreID    int64           `json:"storeId"`
	DeviceID   uint32          `json:"deviceId"`
	Name       string          `json:"name"`
	ExporterIP string          `json:"exporterIp"`
	State      ComplianceState `json:"state"`
	Answerable bool            `json:"answerable"`
	Flows      uint64          `json:"flows"`
	Translated uint64          `json:"translated"`
	WithPort   uint64          `json:"withPort"`
	NoNATField uint64          `json:"noNatField"`
	// NoNATDropped counts flows the dataplane discarded before storage for
	// carrying no post-NAT address — the only trace such a device leaves.
	NoNATDropped uint64 `json:"noNatDropped"`
	SameAsSrc    uint64 `json:"sameAsSrc"`
	PrivateSrc   uint64 `json:"privateSrc"`
	// TranslatedPct is translated/flows as a percentage — an efficiency hint
	// (how much of what this device sends is IPDR-relevant), not a verdict.
	TranslatedPct float64   `json:"translatedPct"`
	LastFlow      time.Time `json:"lastFlow"`
	Detail        string    `json:"detail"` // what was observed
	Remedy        string    `json:"remedy"` // what must change, and where
}

// ComplianceReport is the tenant-scoped audit surfaced to the console and used
// by the monitor.
type ComplianceReport struct {
	GeneratedAt   time.Time          `json:"generatedAt"`
	WindowMins    int                `json:"windowMins"`
	Devices       []DeviceCompliance `json:"devices"`
	Answerable    int                `json:"answerable"`
	Failing       int                `json:"failing"`
	RetentionDays int                `json:"retentionDays"`
	RetentionMin  int                `json:"retentionMin"`
	RetentionOK   bool               `json:"retentionOk"`
	// MissingDays lists dates inside the retention window that hold zero
	// records. A lawful request landing on such a date can only be answered
	// "no records exist", so these are reported as first-class findings.
	MissingDays []string `json:"missingDays"`
	// UnanswerableDays lists dates that hold records but no translation at all.
	// These do not show up as missing — the day looks fully populated — yet a
	// request about them is just as unanswerable. Observed in the field: a day
	// with 6.3M stored flows and 127 translations.
	UnanswerableDays []string  `json:"unanswerableDays"`
	Days             []DayIPDR `json:"days"`
	Available        bool      `json:"available"`
}

const (
	// complianceWindowMins is the look-back used when grading a device. It must
	// span a full day: subscriber NAT activity is strongly diurnal, and a real
	// exporter can legitimately translate nothing for hours. Measured on a live
	// CGNAT device, hourly translations swung from ~41,000 at 16:00 to 2 at
	// 22:00 — a one-hour window would have graded it "no NAT fields" overnight
	// and paged someone every night. Capability is a question about the day.
	complianceWindowMins = 24 * 60
	// retentionMinDays is the retention floor this build audits against.
	// CERT-In Direction 20(3)/2022 requires ICT system logs to be kept for a
	// rolling 180 days; an ISP's own licence terms may require longer, so this
	// is a floor and the operator can raise the configured retention above it.
	retentionMinDays = 180
	// complianceTickEvery is how often the monitor re-grades every device.
	complianceTickEvery = 15 * time.Minute
	// dayCountTimeout bounds the per-day row count. It reads only the partition
	// key, so this is generous by a wide margin — measured at 0.08s over 51 days
	// on the busiest box in the fleet.
	dayCountTimeout = 20 * time.Second
	// complianceGrace is how long a newly-registered device may stay ungraded
	// before silence itself becomes an alert. A real exporter starts inside a
	// minute; a mis-configured one never does.
	complianceGrace = 2 * time.Hour
)

// DayIPDR is one day's flow count alongside how many of those flows carry a NAT
// translation — the only ones that can answer a lawful request.
type DayIPDR struct {
	Date       string `json:"date"`
	Flows      uint64 `json:"flows"`
	Translated uint64 `json:"translated"`
	// TranslatedKnown says whether Translated was actually measured. Counting
	// translations reads two IP columns across the day; on a collector storing
	// hundreds of millions of rows a day that does not finish, and reporting an
	// unmeasured day as "0 translations" would brand a healthy day unanswerable.
	TranslatedKnown bool `json:"translatedKnown"`
}

// dayFlowCounts returns the number of stored rows per retained day, newest
// first. Free: event_date is the partition key, so ClickHouse answers from part
// metadata — measured at 0.08s over 51 days on the busiest box in the fleet.
// This is the query that answers "which retained dates hold no records at all",
// and it must never be coupled to anything expensive.
func (r *FlowReader) dayFlowCounts(ctx context.Context, ispID uint32) ([]DayIPDR, error) {
	where, args := "1", []any(nil)
	if ispID != 0 {
		where, args = "isp_id = ?", []any{ispID}
	}
	cctx, cancel := context.WithTimeout(ctx, dayCountTimeout)
	defer cancel()
	rows, err := r.conn.Query(cctx, fmt.Sprintf(
		`SELECT toString(event_date), count() FROM %s.flow_logs WHERE %s
		 GROUP BY event_date ORDER BY event_date DESC`, r.db, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayIPDR
	for rows.Next() {
		var d DayIPDR
		if err := rows.Scan(&d.Date, &d.Flows); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TranslationsByDay returns per-day flow counts over the whole retained window,
// newest first, with translation counts attached where they have been measured.
//
// The two halves have wildly different costs and so are sourced differently.
// Counting rows per day is free and always runs here. Counting *translations*
// per day reads two IP columns across every row: about 1.5-6.4s for one day on
// the busiest collector, but over 170s for a 51-day window, which ClickHouse
// refuses up front as too slow. A 15-minute audit cannot pay that, so it does
// not try — RunIPDRDayBackfill measures one day at a time and stores the result,
// and this reads it back in microseconds.
//
// A day the backfill has not reached yet comes back TranslatedKnown=false. That
// is not the same as zero, and nothing downstream may treat it as zero: the
// console renders it "not measured" and unanswerableDays skips it, because a
// measurement that has not happened is not evidence of a legal gap.
func (r *FlowReader) TranslationsByDay(ctx context.Context, ispID uint32) ([]DayIPDR, error) {
	out, err := r.dayFlowCounts(ctx, ispID)
	if err != nil {
		return nil, err
	}
	stored, err := r.storedIPDRDays(ctx, ispID)
	if err != nil {
		return out, errTranslationsUnmeasured{err}
	}
	for i := range out {
		// Only trust a stored figure that was computed against the same number of
		// rows the day holds now. A day that grew since it was measured (today, or
		// an old day that received late-arriving records) would otherwise report a
		// stale translation count as fact until the backfill catches up.
		if s, ok := stored[out[i].Date]; ok && s.Flows == out[i].Flows {
			out[i].Translated, out[i].TranslatedKnown = s.Translated, true
		}
	}
	return out, nil
}

// errTranslationsUnmeasured reports that the per-day counts came back without
// translation figures. The day counts in the same result are still good, so the
// caller uses them and only logs this — but it must never be swallowed silently:
// "not measured" showing up across a whole console page needs a reason in the
// log, or the next person assumes the exporters stopped translating.
type errTranslationsUnmeasured struct{ err error }

func (e errTranslationsUnmeasured) Error() string {
	return "per-day translation counts unmeasured: " + e.err.Error()
}
func (e errTranslationsUnmeasured) Unwrap() error { return e.err }

// unanswerableDays lists days that stored flows but recorded no translation at
// all. Deliberately a zero test, not a threshold: a quiet day is not a fault,
// but a day that translated nothing cannot answer anything.
func unanswerableDays(days []DayIPDR) []string {
	var out []string
	for _, d := range days {
		// An unmeasured day is not evidence of anything. Calling it unanswerable
		// because a query timed out would put real dates in front of an operator
		// as legal gaps that do not exist.
		if d.TranslatedKnown && d.Flows > 0 && d.Translated == 0 {
			out = append(out, d.Date)
		}
	}
	sort.Strings(out)
	return out
}

// TranslationStats returns per-device translation counters over the window.
// ispID 0 means all tenants (director scope).
func (r *FlowReader) TranslationStats(ctx context.Context, ispID uint32, window time.Duration) (map[uint32]DeviceFlowStats, error) {
	mins := int(window.Minutes())
	if mins < 1 {
		mins = 1
	}
	// event_date is the IST date partition key; bounding it lets ClickHouse
	// prune whole partitions instead of scanning every day's parts. Two days of
	// margin covers any window that straddles IST midnight.
	where := fmt.Sprintf("event_date >= today() - 2 AND flow_start > now() - INTERVAL %d MINUTE", mins)
	var args []any
	if ispID != 0 {
		where += " AND isp_id = ?"
		args = append(args, ispID)
	}
	const privateSrc = `isIPAddressInRange(toString(src_ip),'10.0.0.0/8')
		OR isIPAddressInRange(toString(src_ip),'172.16.0.0/12')
		OR isIPAddressInRange(toString(src_ip),'192.168.0.0/16')
		OR isIPAddressInRange(toString(src_ip),'100.64.0.0/10')`
	const translated = `nat_public_ip != toIPv4('0.0.0.0') AND nat_public_ip != src_ip`
	q := fmt.Sprintf(`SELECT device_id,
			count(),
			countIf(%[1]s),
			countIf(nat_public_ip = toIPv4('0.0.0.0')),
			countIf(nat_public_ip = src_ip),
			countIf((%[1]s) AND nat_public_port != 0),
			countIf(%[2]s),
			max(flow_start)
		FROM %[3]s.flow_logs WHERE %[4]s GROUP BY device_id`, translated, privateSrc, r.db, where)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32]DeviceFlowStats{}
	for rows.Next() {
		var s DeviceFlowStats
		if err := rows.Scan(&s.DeviceID, &s.Flows, &s.Translated, &s.NoNATField,
			&s.SameAsSrc, &s.WithPort, &s.PrivateSrc, &s.LastFlow); err != nil {
			return nil, err
		}
		out[s.DeviceID] = s
	}
	return out, rows.Err()
}

// gradeDevice grades one device from its counters. Pure so the decision table
// is testable without a database.
func gradeDevice(d store.Device, st DeviceFlowStats, sig DeviceSignal) DeviceCompliance {
	c := DeviceCompliance{
		StoreID: d.ID, DeviceID: d.DeviceID, Name: d.Name, ExporterIP: d.ExporterIP,
		Flows: st.Flows, Translated: st.Translated, WithPort: st.WithPort,
		NoNATField: st.NoNATField, SameAsSrc: st.SameAsSrc, PrivateSrc: st.PrivateSrc,
		LastFlow: st.LastFlow, NoNATDropped: sig.NoNATDropped,
	}
	// Drop evidence counts as proof of life in every branch, not just the one
	// where storage is empty. During the window after the no-translation rule
	// starts biting, a device still has old stored rows whose newest timestamp
	// is frozen at the moment the rule took effect — reporting that as "last
	// flow" would make a busy exporter look like it stopped hours ago.
	if sig.LastFlow.After(c.LastFlow) {
		c.LastFlow = sig.LastFlow
	}
	if st.Flows > 0 {
		c.TranslatedPct = float64(st.Translated) / float64(st.Flows) * 100
	}
	switch {
	case !d.Enabled:
		c.State = CompDisabled
		c.Detail = "Device is disabled; not graded."
		c.Remedy = ""

	case st.Flows == 0 && sig.NoNATDropped > 0:
		// Storage is empty, but the dataplane saw traffic and threw all of it
		// away for carrying no translation. Without this branch the device is
		// indistinguishable from a dead one and the operator hunts a link fault.
		c.State = CompNoNATFields
		c.Detail = fmt.Sprintf("This exporter IS sending — %s flows arrived and were discarded, every one of them carrying no post-NAT address. Nothing was stored, so it appears silent everywhere else.",
			human(sig.NoNATDropped))
		c.Remedy = "The exporter is sending plain traffic flows, not NAT translation records. Enable CGNAT/NAT flow logging on the device so it emits " +
			"postNATSourceIPv4Address (IE 225) and postNAPTSourceTransportPort (IE 227), ideally with natEvent (IE 230) for allocation/release. " +
			"NetFlow v5 can never satisfy this — it has no post-NAT fields at all; export v9 or IPFIX."

	case st.Flows == 0:
		c.State = CompSilent
		c.Detail = "No flows stored from this exporter, and none arrived to be dropped either."
		c.Remedy = fmt.Sprintf("Confirm the exporter is sending to this collector and that %s is its configured source address. "+
			"On the exporter, the export source address must be its own IP — a 0.0.0.0 source makes this collector reject the packets as an unknown exporter.", d.ExporterIP)

	case st.Translated == 0 && st.NoNATField >= st.SameAsSrc:
		c.State = CompNoNATFields
		c.Detail = fmt.Sprintf("%s flows stored, but the post-NAT address is unset (0.0.0.0) on %s of them — no translation is being logged.",
			human(st.Flows), human(st.NoNATField))
		c.Remedy = "The exporter is sending plain traffic flows, not NAT translation records. Enable CGNAT/NAT flow logging on the device so it emits " +
			"postNATSourceIPv4Address (IE 225) and postNAPTSourceTransportPort (IE 227), ideally with natEvent (IE 230) for allocation/release. " +
			"A traffic-flow or log-server export alone will never carry these fields."

	case st.Translated == 0:
		c.State = CompNoTranslation
		c.Detail = fmt.Sprintf("%s flows stored, but the post-NAT address always equals the source on %s of them — nothing was actually translated.",
			human(st.Flows), human(st.SameAsSrc))
		c.Remedy = "These are untranslated flows (inbound or transit traffic), which carry no subscriber mapping. Export the subscriber-side, " +
			"post-NAT direction from this device so translations are recorded."

	case st.WithPort == 0:
		c.State = CompNoPort
		c.Detail = fmt.Sprintf("%s translations logged, but the post-NAT port is zero on every one of them.", human(st.Translated))
		c.Remedy = "Under CGNAT a single public IP is shared by many subscribers and the port is the only discriminator, so an IP-only record " +
			"cannot identify one. Enable post-NAT port logging (postNAPTSourceTransportPort, IE 227) on the exporter."

	case st.WithPort < st.Translated:
		c.State = CompPartialPort
		c.Detail = fmt.Sprintf("%s of %s translations carry a post-NAT port; the rest are IP-only and cannot identify a subscriber.",
			human(st.WithPort), human(st.Translated))
		c.Remedy = "Check the exporter's template: some flow records are omitting postNAPTSourceTransportPort (IE 227)."

	default:
		c.State = CompOK
		c.Detail = fmt.Sprintf("%s translations with ports logged in the window.", human(st.Translated))
		if st.Flows > 0 && c.TranslatedPct < 5 {
			// Answerable, but the device is spending most of its export budget
			// (and this collector's disk) on records of no IPDR value.
			c.Detail += fmt.Sprintf(" Note: only %.1f%% of this device's stored flows are translations — the rest is inbound or transit traffic that cannot answer a lawful request.", c.TranslatedPct)
		}
	}
	// Stored counts describe history; the drop counter describes now. When both
	// exist, say so — otherwise the numbers on screen quietly stop moving and
	// nothing explains why.
	if sig.NoNATDropped > 0 && st.Flows > 0 {
		c.Detail += fmt.Sprintf(" Since this collector last started, %s further flows from this exporter were discarded before storage for the same reason, so the stored counts above no longer grow.",
			human(sig.NoNATDropped))
	}
	c.Answerable = c.State.answerable()
	return c
}

// human formats a count with thousands separators.
func human(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// missingDays returns dates inside the retention window that hold zero records.
// A gap is only meaningful between the oldest and newest day actually held, so
// days before ingestion started are not reported as missing.
func missingDays(days []DayIPDR, now time.Time) []string {
	if len(days) < 2 {
		return nil
	}
	have := make(map[string]bool, len(days))
	var oldest, newest time.Time
	for _, d := range days {
		t, err := time.Parse("2006-01-02", d.Date)
		if err != nil {
			continue
		}
		have[d.Date] = true
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		if newest.IsZero() || t.After(newest) {
			newest = t
		}
	}
	if oldest.IsZero() || newest.IsZero() {
		return nil
	}
	var out []string
	for t := oldest; !t.After(newest); t = t.AddDate(0, 0, 1) {
		if k := t.Format("2006-01-02"); !have[k] {
			out = append(out, k)
		}
	}
	return out
}

// ComplianceAudit grades every device visible to ispID (0 = all tenants).
func (s *Server) ComplianceAudit(ctx context.Context, ispID uint32) ComplianceReport {
	rep := ComplianceReport{
		GeneratedAt: time.Now(), WindowMins: complianceWindowMins,
		RetentionMin: retentionMinDays, Devices: []DeviceCompliance{},
	}
	rep.RetentionDays = s.retDays()
	if rep.RetentionDays == 0 {
		rep.RetentionDays = 180
	}
	rep.RetentionOK = rep.RetentionDays >= retentionMinDays
	if s.flows == nil {
		return rep
	}
	rep.Available = true

	devs, err := s.store.ListDevices(ctx, ispID)
	if err != nil {
		s.log.Warn("compliance: device list failed", "error", err)
		return rep
	}
	// Each query gets its own deadline. Sharing one budget meant the per-day
	// scan inherited whatever the per-device query left over — a few seconds
	// against the full retention window — so on the busy boxes it timed out on
	// essentially every cycle and the retention-gap detection silently never
	// ran. Observed 2026-08-26: 35-36 failures in 26h on box2 and box3.
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stats, err := s.flows.TranslationStats(qctx, ispID, complianceWindowMins*time.Minute)
	if err != nil {
		s.log.Warn("compliance: translation stats failed", "error", err)
		return rep
	}
	sigs := s.deviceSignals()
	for _, d := range devs {
		c := gradeDevice(d, stats[d.DeviceID], sigs[d.DeviceID])
		if c.State == CompDisabled {
			rep.Devices = append(rep.Devices, c)
			continue
		}
		if c.Answerable {
			rep.Answerable++
		} else {
			rep.Failing++
		}
		rep.Devices = append(rep.Devices, c)
	}
	// Failing devices first, then by name, so the console leads with problems.
	sort.SliceStable(rep.Devices, func(i, j int) bool {
		a, b := rep.Devices[i], rep.Devices[j]
		if a.Answerable != b.Answerable {
			return !a.Answerable
		}
		return a.Name < b.Name
	})
	days, err := s.flows.TranslationsByDay(ctx, ispID)
	var unmeasured errTranslationsUnmeasured
	switch {
	case errors.As(err, &unmeasured):
		// Day counts are usable; only the translation figures are missing.
		s.log.Warn("compliance: per-day translation counts unavailable on this collector; gap detection still ran", "error", unmeasured.err)
	case err != nil:
		s.log.Warn("compliance: per-day query failed", "error", err)
		return rep
	}
	rep.Days = days
	rep.MissingDays = missingDays(days, time.Now())
	rep.UnanswerableDays = unanswerableDays(days)
	return rep
}

// compState is the monitor's memory between ticks: the last state alerted on
// per device, so transitions email once rather than every tick.
type compState struct {
	state     ComplianceState
	firstSeen time.Time
	lastAlert time.Time
}

// RunComplianceMonitor re-grades every device on a slow cadence and emails when
// one stops being able to answer a lawful request (or, for a newly-registered
// device, never starts). This is the guard for the failure the liveness monitor
// cannot see: flows arriving, rows stored, nothing of legal value logged.
func (s *Server) RunComplianceMonitor(ctx context.Context) {
	if s.flows == nil {
		return
	}
	first := time.NewTimer(3 * time.Minute) // let flows arrive after a restart
	defer first.Stop()
	tick := time.NewTicker(complianceTickEvery)
	defer tick.Stop()
	seen := map[uint32]*compState{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			s.complianceTick(ctx, seen)
		case <-tick.C:
			s.complianceTick(ctx, seen)
		}
	}
}

func (s *Server) complianceTick(ctx context.Context, seen map[uint32]*compState) {
	rep := s.ComplianceAudit(ctx, 0)
	if !rep.Available {
		return
	}
	now := time.Now()
	remind := s.remindInterval()
	var alerts []DeviceCompliance
	live := map[uint32]bool{}
	for _, c := range rep.Devices {
		if c.State == CompDisabled {
			continue
		}
		live[c.DeviceID] = true
		st, ok := seen[c.DeviceID]
		if !ok {
			st = &compState{state: c.State, firstSeen: now}
			seen[c.DeviceID] = st
			// A device that is already answerable needs no grace period and no
			// alert; one that is not gets until complianceGrace to come good.
			if c.Answerable {
				continue
			}
			continue
		}
		if c.Answerable {
			st.state, st.lastAlert = c.State, time.Time{}
			continue
		}
		// Not answerable. Alert on a state change, or once the grace period has
		// passed for a device that has never been good, then on the remind cadence.
		changed := st.state != c.State
		graceOver := now.Sub(st.firstSeen) >= complianceGrace
		due := st.lastAlert.IsZero() && graceOver || (!st.lastAlert.IsZero() && now.Sub(st.lastAlert) >= remind)
		if changed || due {
			alerts = append(alerts, c)
			st.lastAlert = now
		}
		st.state = c.State
	}
	for id := range seen { // forget deleted devices
		if !live[id] {
			delete(seen, id)
		}
	}
	if len(alerts) == 0 {
		return
	}
	notifier := s.getNotifier()
	if notifier == nil || !s.CurrentSettings().Notifications.Enabled {
		s.log.Warn("compliance: devices cannot answer lawful requests but alerting is not configured",
			"devices", len(alerts))
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name := s.stats().Name
	subject := fmt.Sprintf("[YesLogs] IPDR COMPLIANCE on %s — %d device(s) cannot answer a lawful request", name, len(alerts))
	if err := notifier(sctx, subject, complianceAlertBody(name, alerts, rep)); err != nil {
		s.log.Error("compliance alert email failed", "error", err)
		return
	}
	s.log.Error("compliance alert sent", "devices", len(alerts))
}

func complianceAlertBody(name string, bad []DeviceCompliance, rep ComplianceReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dataplane %q is storing flows from the devices below, but those records CANNOT answer a lawful\n", name)
	b.WriteString("request of the form \"at time T, which subscriber held public IP X port P?\".\n\n")
	b.WriteString("These devices look healthy in every liveness view — flows are arriving and rows are being\nstored. What is missing is the NAT translation itself.\n\n")
	for _, c := range bad {
		fmt.Fprintf(&b, "  DEVICE: %s (exporter %s, device_id %d)\n", c.Name, c.ExporterIP, c.DeviceID)
		fmt.Fprintf(&b, "  STATUS: %s\n", c.State)
		fmt.Fprintf(&b, "  SEEN:   %s\n", c.Detail)
		if c.Remedy != "" {
			fmt.Fprintf(&b, "  FIX:    %s\n", wrapAt(c.Remedy, 88, "          "))
		}
		b.WriteString("\n")
	}
	if len(rep.MissingDays) > 0 {
		fmt.Fprintf(&b, "ALSO: %d day(s) inside the retention window hold zero records — a request landing on one\n", len(rep.MissingDays))
		b.WriteString("of these dates can only be answered \"no records exist\":\n  ")
		b.WriteString(strings.Join(rep.MissingDays, ", "))
		b.WriteString("\n\n")
	}
	if len(rep.UnanswerableDays) > 0 {
		fmt.Fprintf(&b, "ALSO: %d day(s) stored flows but recorded NO translation at all. These days look fully\n", len(rep.UnanswerableDays))
		b.WriteString("populated in Retention, yet a request about them cannot be answered either:\n  ")
		b.WriteString(strings.Join(rep.UnanswerableDays, ", "))
		b.WriteString("\n\n")
	}
	if !rep.RetentionOK {
		fmt.Fprintf(&b, "ALSO: retention is set to %d days, below the %d-day floor this build audits against.\n\n",
			rep.RetentionDays, rep.RetentionMin)
	}
	b.WriteString("Flow export is fire-and-forget: anything not logged now cannot be recovered later.\n\n")
	b.WriteString("— YesLogs Operations\n")
	return b.String()
}

// wrapAt soft-wraps s at width, indenting continuation lines, so alert emails
// stay readable in a plain-text mail client.
func wrapAt(s string, width int, indent string) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	line := 0
	for i, w := range words {
		if i > 0 && line+1+len(w) > width {
			b.WriteString("\n" + indent)
			line = 0
		} else if i > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

// handleCompliance serves the IPDR compliance audit, tenant-scoped: a director
// sees every device, an ISP admin only their own.
func (s *Server) handleCompliance(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authJSON(w, r)
	if !ok {
		return
	}
	scope := id.ISPID
	if id.isDirector() {
		scope = 0
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.ComplianceAudit(ctx, scope))
}
