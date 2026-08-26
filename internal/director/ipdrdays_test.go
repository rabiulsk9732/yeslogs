package director

import (
	"testing"
)

func TestIPDRDaysNeedingWork(t *testing.T) {
	live := []DayIPDR{
		{Date: "2026-08-26", Flows: 38_404_315},  // today, still growing
		{Date: "2026-08-25", Flows: 379_512_186}, // measured, unchanged
		{Date: "2026-08-24", Flows: 362_254_427}, // measured, but grew since
		{Date: "2026-08-23", Flows: 349_761_546}, // never measured
	}
	stored := map[string]ipdrDayRow{
		"2026-08-26": {Flows: 30_000_000, Translated: 12_000_000},
		"2026-08-25": {Flows: 379_512_186, Translated: 159_933_027},
		"2026-08-24": {Flows: 300_000_000, Translated: 120_000_000},
	}
	got := ipdrDaysNeedingWork(live, stored, 10)
	want := []string{"2026-08-26", "2026-08-24", "2026-08-23"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The budget bounds one pass so the backfill never competes with ingest for
// long, and it must take the newest days first — today is the one an operator
// is most likely to be looking at.
func TestIPDRDaysWorkIsBudgetedNewestFirst(t *testing.T) {
	var live []DayIPDR
	for _, d := range []string{"2026-08-26", "2026-08-25", "2026-08-24", "2026-08-23", "2026-08-22"} {
		live = append(live, DayIPDR{Date: d, Flows: 100})
	}
	got := ipdrDaysNeedingWork(live, map[string]ipdrDayRow{}, 2)
	if len(got) != 2 || got[0] != "2026-08-26" || got[1] != "2026-08-25" {
		t.Fatalf("got %v, want the two newest days", got)
	}
}

// Nothing to do is the steady state, and it must produce no work — a pass that
// always finds something would rescan the whole window forever.
func TestIPDRDaysNoWorkWhenCurrent(t *testing.T) {
	live := []DayIPDR{{Date: "2026-08-25", Flows: 379_512_186}}
	stored := map[string]ipdrDayRow{"2026-08-25": {Flows: 379_512_186, Translated: 159_933_027}}
	if got := ipdrDaysNeedingWork(live, stored, 4); len(got) != 0 {
		t.Fatalf("got %v, want no work", got)
	}
}
