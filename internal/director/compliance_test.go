package director

import (
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func dev(name, ip string, id uint32, enabled bool) store.Device {
	return store.Device{ID: int64(id), DeviceID: id, Name: name, ExporterIP: ip, Enabled: enabled}
}

// The decision table is the whole feature: a wrong grade either hides a device
// that cannot answer a lawful request, or cries wolf about one that can.
func TestGradeDevice(t *testing.T) {
	cases := []struct {
		name       string
		d          store.Device
		st         DeviceFlowStats
		want       ComplianceState
		answerable bool
	}{
		{
			name: "disabled device is not graded",
			d:    dev("off", "10.0.0.1", 1, false),
			st:   DeviceFlowStats{Flows: 500, Translated: 500, WithPort: 500},
			want: CompDisabled, answerable: false,
		},
		{
			name: "no flows at all is silent",
			d:    dev("quiet", "10.0.0.2", 2, true),
			st:   DeviceFlowStats{},
			want: CompSilent, answerable: false,
		},
		{
			// The box4 device-3 case: 1.76M flows/day stored, every one with an
			// unset post-NAT address. Online everywhere, useless for a request.
			name: "flows with post-NAT address unset",
			d:    dev("dandy-bng", "103.204.1.14", 3, true),
			st:   DeviceFlowStats{Flows: 1759160, NoNATField: 1759160},
			want: CompNoNATFields, answerable: false,
		},
		{
			name: "post-NAT address present but never translated",
			d:    dev("transit", "10.0.0.4", 4, true),
			st:   DeviceFlowStats{Flows: 900, SameAsSrc: 900},
			want: CompNoTranslation, answerable: false,
		},
		{
			// Mixed untranslated traffic still grades on which cause dominates,
			// because the remedy differs: missing fields vs wrong direction.
			name: "untranslated, mostly same-as-src, grades as no_translation",
			d:    dev("mixed", "10.0.0.5", 5, true),
			st:   DeviceFlowStats{Flows: 1000, SameAsSrc: 900, NoNATField: 100},
			want: CompNoTranslation, answerable: false,
		},
		{
			name: "untranslated, mostly unset fields, grades as no_nat_fields",
			d:    dev("mixed2", "10.0.0.6", 6, true),
			st:   DeviceFlowStats{Flows: 1000, SameAsSrc: 100, NoNATField: 900},
			want: CompNoNATFields, answerable: false,
		},
		{
			// Public IP without the port cannot identify one subscriber among
			// the many sharing that IP — not answerable, however healthy it looks.
			name: "translations logged without post-NAT port",
			d:    dev("noport", "10.0.0.7", 7, true),
			st:   DeviceFlowStats{Flows: 1000, Translated: 1000, WithPort: 0},
			want: CompNoPort, answerable: false,
		},
		{
			name: "some translations missing the port",
			d:    dev("partial", "10.0.0.8", 8, true),
			st:   DeviceFlowStats{Flows: 1000, Translated: 1000, WithPort: 600},
			want: CompPartialPort, answerable: true,
		},
		{
			// The box4 device-1 case: only ~3% of stored flows are translations,
			// but 224k of them carry ports — requests about those ARE answerable,
			// so a low ratio must not be graded as a failure.
			name: "low translation ratio is still answerable",
			d:    dev("oji-nas", "144.79.198.82", 9, true),
			st:   DeviceFlowStats{Flows: 6839588, Translated: 224242, WithPort: 224242},
			want: CompOK, answerable: true,
		},
		{
			name: "healthy device",
			d:    dev("good", "10.0.0.10", 10, true),
			st:   DeviceFlowStats{Flows: 1000, Translated: 950, WithPort: 950},
			want: CompOK, answerable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gradeDevice(tc.d, tc.st, DeviceSignal{})
			if got.State != tc.want {
				t.Errorf("state = %q, want %q (detail: %s)", got.State, tc.want, got.Detail)
			}
			if got.Answerable != tc.answerable {
				t.Errorf("answerable = %v, want %v", got.Answerable, tc.answerable)
			}
			if got.State != CompOK && got.State != CompDisabled && got.Remedy == "" {
				t.Error("a failing grade must carry a remedy the operator can act on")
			}
		})
	}
}

// A low-ratio device is answerable, but the operator should still be told most
// of its export is not IPDR-relevant — that is a disk and export-budget cost.
func TestGradeDeviceFlagsLowRatioAsANote(t *testing.T) {
	c := gradeDevice(dev("oji", "1.2.3.4", 1, true),
		DeviceFlowStats{Flows: 6839588, Translated: 224242, WithPort: 224242}, DeviceSignal{})
	if c.State != CompOK || !c.Answerable {
		t.Fatalf("must stay answerable, got %q", c.State)
	}
	if !strings.Contains(c.Detail, "%") || !strings.Contains(c.Detail, "transit") {
		t.Errorf("detail should note the low IPDR-relevant ratio, got: %s", c.Detail)
	}
	if got := c.TranslatedPct; got < 3.2 || got > 3.4 {
		t.Errorf("TranslatedPct = %.2f, want ~3.28", got)
	}
}

func TestGradeDeviceNoLowRatioNoteWhenHealthy(t *testing.T) {
	c := gradeDevice(dev("good", "1.2.3.4", 1, true),
		DeviceFlowStats{Flows: 1000, Translated: 950, WithPort: 950}, DeviceSignal{})
	if strings.Contains(c.Detail, "transit") {
		t.Errorf("healthy device should not carry the low-ratio note, got: %s", c.Detail)
	}
}

func TestMissingDays(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		days []DayIPDR
		want []string
	}{
		{
			name: "continuous window has no gaps",
			days: []DayIPDR{{Date: "2026-08-25"}, {Date: "2026-08-24"}, {Date: "2026-08-23"}},
			want: nil,
		},
		{
			// The box3 case: an outage swallowed three days that no longer exist
			// anywhere. A request landing there can only be answered "no records".
			name: "interior gap is reported",
			days: []DayIPDR{{Date: "2026-08-25"}, {Date: "2026-08-21"}, {Date: "2026-08-20"}},
			want: []string{"2026-08-22", "2026-08-23", "2026-08-24"},
		},
		{
			name: "single day cannot have a gap",
			days: []DayIPDR{{Date: "2026-08-25"}},
			want: nil,
		},
		{
			name: "empty input",
			days: nil,
			want: nil,
		},
		{
			name: "unparseable dates are skipped, not fatal",
			days: []DayIPDR{{Date: "not-a-date"}, {Date: "2026-08-25"}, {Date: "2026-08-23"}},
			want: []string{"2026-08-24"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := missingDays(tc.days, now)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// Days before ingestion started must not be reported as missing — only real
// holes between the oldest and newest day actually held.
func TestMissingDaysIgnoresDaysBeforeIngestionStarted(t *testing.T) {
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	days := []DayIPDR{{Date: "2026-08-25"}, {Date: "2026-08-24"}}
	if got := missingDays(days, now); len(got) != 0 {
		t.Errorf("expected no gaps for a two-day-old install, got %v", got)
	}
}

func TestHuman(t *testing.T) {
	cases := map[uint64]string{
		0: "0", 7: "7", 42: "42", 999: "999", 1000: "1,000",
		1759160: "1,759,160", 6839588: "6,839,588", 1000000: "1,000,000",
	}
	for in, want := range cases {
		if got := human(in); got != want {
			t.Errorf("human(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestWrapAt(t *testing.T) {
	got := wrapAt("one two three four five six", 10, "  ")
	for _, line := range strings.Split(got, "\n") {
		if len(strings.TrimLeft(line, " ")) > 10 && strings.Contains(strings.TrimSpace(line), " ") {
			t.Errorf("line exceeds width: %q", line)
		}
	}
	if strings.Contains(got, "\n") && !strings.Contains(got, "\n  ") {
		t.Error("continuation lines must be indented")
	}
	if wrapAt("", 10, "  ") != "" {
		t.Error("empty input should produce empty output")
	}
}

// The alert body is what an operator acts on at 3am; it must name the device,
// say what was seen, and say what to change.
func TestComplianceAlertBody(t *testing.T) {
	bad := []DeviceCompliance{
		gradeDevice(dev("dandy-bng", "103.204.1.14", 3, true),
			DeviceFlowStats{Flows: 1759160, NoNATField: 1759160}, DeviceSignal{}),
	}
	rep := ComplianceReport{
		RetentionDays: 90, RetentionMin: 180, RetentionOK: false,
		MissingDays: []string{"2026-07-07", "2026-07-08"},
	}
	body := complianceAlertBody("natlog-01", bad, rep)
	for _, want := range []string{
		"dandy-bng", "103.204.1.14", "no_nat_fields",
		"IE 225", "IE 227", // the operator needs the field numbers to hand on
		"2026-07-07",      // the gap
		"90 days",         // the retention shortfall
		"fire-and-forget", // why it cannot be fixed retroactively
	} {
		if !strings.Contains(body, want) {
			t.Errorf("alert body missing %q\n---\n%s", want, body)
		}
	}
}

func TestComplianceAlertBodyOmitsCleanSections(t *testing.T) {
	bad := []DeviceCompliance{gradeDevice(dev("d", "1.2.3.4", 1, true), DeviceFlowStats{}, DeviceSignal{})}
	rep := ComplianceReport{RetentionDays: 365, RetentionMin: 180, RetentionOK: true}
	body := complianceAlertBody("natlog-01", bad, rep)
	if strings.Contains(body, "below the") {
		t.Error("must not report a retention shortfall when retention is fine")
	}
	if strings.Contains(body, "hold zero records") {
		t.Error("must not report gaps when there are none")
	}
}

func TestAnswerableStates(t *testing.T) {
	answerable := map[ComplianceState]bool{
		CompOK: true, CompPartialPort: true,
		CompNoPort: false, CompNoNATFields: false, CompNoTranslation: false,
		CompSilent: false, CompDisabled: false,
	}
	for st, want := range answerable {
		if got := st.answerable(); got != want {
			t.Errorf("%q.answerable() = %v, want %v", st, got, want)
		}
	}
}

// A day can be full of stored flows and still answer nothing. This is the case
// that Retention's per-day counts cannot show: the day looks healthy there.
func TestUnanswerableDays(t *testing.T) {
	days := []DayIPDR{
		{Date: "2026-08-25", Flows: 8778667, Translated: 246216, TranslatedKnown: true}, // healthy
		{Date: "2026-08-24", Flows: 8064258, Translated: 284688, TranslatedKnown: true}, // healthy
		{Date: "2026-08-23", Flows: 6345272, Translated: 0, TranslatedKnown: true},      // rows, no translations
		{Date: "2026-08-22", Flows: 0, Translated: 0, TranslatedKnown: true},            // no rows at all
	}
	got := unanswerableDays(days)
	if len(got) != 1 || got[0] != "2026-08-23" {
		t.Fatalf("got %v, want [2026-08-23]", got)
	}
}

// A day with zero rows is a missing day, not an unanswerable one — reporting it
// twice would double-count the same finding.
func TestUnanswerableDaysIgnoresEmptyDays(t *testing.T) {
	if got := unanswerableDays([]DayIPDR{{Date: "2026-08-22", Flows: 0, TranslatedKnown: true}}); len(got) != 0 {
		t.Errorf("a day with no rows is a missing day, not unanswerable: got %v", got)
	}
}

// Deliberately a zero test: a genuinely quiet day must not be flagged, or the
// alert becomes noise and gets ignored.
func TestUnanswerableDaysAllowsQuietDays(t *testing.T) {
	days := []DayIPDR{{Date: "2026-08-23", Flows: 6345272, Translated: 127, TranslatedKnown: true}}
	if got := unanswerableDays(days); len(got) != 0 {
		t.Errorf("a low-but-nonzero day must not be flagged: got %v", got)
	}
}

func TestComplianceAlertBodyReportsUnanswerableDays(t *testing.T) {
	bad := []DeviceCompliance{gradeDevice(dev("d", "1.2.3.4", 1, true), DeviceFlowStats{}, DeviceSignal{})}
	rep := ComplianceReport{
		RetentionDays: 365, RetentionMin: 180, RetentionOK: true,
		UnanswerableDays: []string{"2026-08-23"},
	}
	body := complianceAlertBody("natlog-01", bad, rep)
	if !strings.Contains(body, "2026-08-23") || !strings.Contains(body, "NO translation") {
		t.Errorf("alert must report days that stored flows but translated nothing:\n%s", body)
	}
}

// The grading window must span a full day. Subscriber NAT activity is diurnal:
// a live device measured 41k translations at 16:00 and 2 at 22:00, so a short
// window would grade a healthy exporter as broken every night.
func TestComplianceWindowSpansAFullDay(t *testing.T) {
	if complianceWindowMins < 24*60 {
		t.Fatalf("grading window is %d min; a sub-day window false-alarms on the overnight trough", complianceWindowMins)
	}
}

// The hard-coded no-translation rule means a device exporting traffic rather
// than NAT stores nothing at all. Storage alone cannot then tell it apart from
// a dead exporter — the dataplane's dropped-flow evidence has to, or the
// operator is sent hunting a link fault that does not exist.
func TestDroppedEverythingIsNotSilent(t *testing.T) {
	d := dev("dandy-bng", "103.204.1.14", 3, true)
	seen := time.Date(2026, 8, 25, 23, 30, 0, 0, time.UTC)

	dead := gradeDevice(d, DeviceFlowStats{}, DeviceSignal{})
	if dead.State != CompSilent {
		t.Errorf("no flows and no evidence = silent, got %q", dead.State)
	}

	alive := gradeDevice(d, DeviceFlowStats{}, DeviceSignal{NoNATDropped: 2268856, LastFlow: seen})
	if alive.State != CompNoNATFields {
		t.Fatalf("state = %q, want %q — a live exporter must not read as silent", alive.State, CompNoNATFields)
	}
	if alive.NoNATDropped != 2268856 {
		t.Errorf("NoNATDropped = %d, want 2268856", alive.NoNATDropped)
	}
	if !strings.Contains(alive.Detail, "IS sending") {
		t.Errorf("detail must say the exporter is alive, got: %s", alive.Detail)
	}
	if !strings.Contains(alive.Remedy, "IE 225") || !strings.Contains(alive.Remedy, "v5") {
		t.Errorf("remedy must name the fields and the v5 dead end, got: %s", alive.Remedy)
	}
}

// Stored rows win when they exist: evidence of drops must not override a device
// that is actually logging translations.
func TestStoredFlowsTakePrecedenceOverDropEvidence(t *testing.T) {
	c := gradeDevice(dev("oji", "1.2.3.4", 1, true),
		DeviceFlowStats{Flows: 1000, Translated: 900, WithPort: 900},
		DeviceSignal{NoNATDropped: 50})
	if c.State != CompOK || !c.Answerable {
		t.Errorf("state = %q answerable = %v, want ok/true", c.State, c.Answerable)
	}
}

// Drop evidence must be reported whatever the grade. A device with stored rows
// from before the no-translation rule took effect still falls through to the
// stored-row branches, and the operator needs to see that it is also being
// dropped live — and needs a last-flow time that has not frozen.
func TestDropEvidenceSurfacesInEveryBranch(t *testing.T) {
	stale := time.Now().Add(-6 * time.Hour)
	live := time.Now().Add(-30 * time.Second)
	st := DeviceFlowStats{Flows: 1000, NoNATField: 1000, LastFlow: stale}
	sig := DeviceSignal{NoNATDropped: 4200, LastFlow: live}

	c := gradeDevice(store.Device{DeviceID: 3, Enabled: true}, st, sig)
	if c.State != CompNoNATFields {
		t.Errorf("State = %q, want %q", c.State, CompNoNATFields)
	}
	if c.NoNATDropped != 4200 {
		t.Errorf("NoNATDropped = %d, want 4200 — drop evidence lost outside the empty-storage branch", c.NoNATDropped)
	}
	if !c.LastFlow.Equal(live) {
		t.Errorf("LastFlow = %v, want the live drop at %v; a frozen stored timestamp reads as a dead exporter", c.LastFlow, live)
	}
}

// The reverse: stored rows newer than the last drop must win, so a device that
// started logging translations again is not pinned to old drop evidence.
func TestStoredTimestampWinsWhenNewer(t *testing.T) {
	newer := time.Now().Add(-1 * time.Minute)
	older := time.Now().Add(-2 * time.Hour)
	c := gradeDevice(store.Device{DeviceID: 3, Enabled: true},
		DeviceFlowStats{Flows: 100, Translated: 100, WithPort: 100, LastFlow: newer},
		DeviceSignal{NoNATDropped: 5, LastFlow: older})
	if !c.LastFlow.Equal(newer) {
		t.Errorf("LastFlow = %v, want the newer stored flow %v", c.LastFlow, newer)
	}
}

// A day whose translation count could not be measured must never be reported as
// a legal gap. The count times out on the busiest collectors, and an operator
// shown a list of "unanswerable" dates has no way to tell a real gap from a
// query that gave up.
func TestUnmeasuredDaysAreNotUnanswerable(t *testing.T) {
	days := []DayIPDR{
		{Date: "2026-08-25", Flows: 200_000_000},                                   // not measured
		{Date: "2026-08-24", Flows: 6345272, Translated: 0, TranslatedKnown: true}, // measured, genuinely empty
	}
	got := unanswerableDays(days)
	if len(got) != 1 || got[0] != "2026-08-24" {
		t.Fatalf("got %v, want only the measured day [2026-08-24]", got)
	}
}
