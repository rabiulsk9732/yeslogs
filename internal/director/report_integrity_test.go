package director

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func TestHotWhereRequireNAT(t *testing.T) {
	f := SearchFilter{ISPID: 2, DeviceID: 4, RequireNAT: true}
	where, args, ok := hotWhere(f)
	if !ok {
		t.Fatal("hotWhere rejected a device-scoped report filter")
	}
	for _, want := range []string{
		"device_id = ?",
		"nat_public_ip != toIPv4('0.0.0.0')",
		"nat_public_ip != src_ip",
		"isp_id = ?",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("hot WHERE missing %q: %s", want, where)
		}
	}
	if len(args) != 2 || args[0] != uint32(4) || args[1] != uint32(2) {
		t.Fatalf("unexpected hot args: %#v", args)
	}
}

func TestColdWhereRequireNATUsesTenantPathScope(t *testing.T) {
	f := SearchFilter{ISPID: 2, DeviceID: 4, RequireNAT: true}
	conds, args, ok := coldWhere(f, "flow_start")
	if !ok {
		t.Fatal("coldWhere rejected a device-scoped report filter")
	}
	where := strings.Join(conds, " AND ")
	for _, want := range []string{
		"device_id = ?",
		"nat_public_ip != ''",
		"nat_public_ip != '0.0.0.0'",
		"nat_public_ip != src_ip",
	} {
		if !strings.Contains(where, want) {
			t.Errorf("cold WHERE missing %q: %s", want, where)
		}
	}
	if strings.Contains(where, "isp_id") {
		t.Fatalf("cold WHERE must not duplicate the path tenant scope: %s", where)
	}
	if len(args) != 1 || args[0] != uint32(4) {
		t.Fatalf("unexpected cold args: %#v", args)
	}

	c := ColdS3{Endpoint: "https://objects.example", Bucket: "archive", Prefix: "natlog"}
	url := c.urlForDays(f.ISPID, []string{"2026-07-08", "2026-07-09"})
	if !strings.Contains(url, "/isp_id=2/") {
		t.Fatalf("tenant scope missing from cold URL: %s", url)
	}
}

func TestLogsISPDeviceCascadeIsEmbedded(t *testing.T) {
	b, err := consoleFS.ReadFile("web/console/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{
		`Select an ISP first`,
		`devs.filter(d => +d.ISPID === ispID)`,
		`S.lq = null`,
		`if (reqID !== S.logReq) return`,
		`Shared local collector`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Logs ISP/device cascade missing %q", want)
		}
	}
}

func TestPlanColdSearchIsBoundedAndHotFirst(t *testing.T) {
	from := time.Date(2026, 7, 8, 0, 0, 0, 0, istLoc)
	to := time.Date(2026, 7, 9, 23, 59, 0, 0, istLoc)
	twoDays := []string{"2026-07-09", "2026-07-08"}

	use, err := planColdSearch(SearchFilter{}, twoDays, 0)
	if err != nil || use {
		t.Fatalf("no-time browsing must stay hot-only: use=%v err=%v", use, err)
	}

	use, err = planColdSearch(SearchFilter{From: from, To: to}, twoDays, countCap)
	if err != nil || use {
		t.Fatalf("a capped hot result must short-circuit cold: use=%v err=%v", use, err)
	}

	use, err = planColdSearch(SearchFilter{From: from, To: to}, twoDays, 0)
	if err != nil || !use {
		t.Fatalf("bounded archived-only search must use cold: use=%v err=%v", use, err)
	}

	if _, err = planColdSearch(SearchFilter{From: from}, twoDays, 0); err == nil {
		t.Fatal("one-sided archived range must be rejected")
	}
	use, err = planColdSearch(
		SearchFilter{From: from.AddDate(-3, 0, 0), To: to},
		append(twoDays, "2026-07-07"),
		0,
	)
	if err != nil || !use {
		t.Fatalf("multi-year archived range must remain queryable: use=%v err=%v", use, err)
	}
}

func TestWalkColdDaysNewestFirstAndStopsWhenFull(t *testing.T) {
	var calls []string
	rows, scanned, capped, err := walkColdDays(
		context.Background(),
		[]string{"2024-01-01", "2026-07-09", "2025-04-03"},
		2,
		func(_ context.Context, day string, limit int) ([]natRecord, error) {
			calls = append(calls, day)
			return []natRecord{{Time: day + " 23:59:59"}, {Time: day + " 23:59:58"}}[:limit], nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if scanned != 1 || len(calls) != 1 || calls[0] != "2026-07-09" {
		t.Fatalf("expected only newest partition to be scanned, calls=%v scanned=%d", calls, scanned)
	}
	if len(rows) != 2 || !capped {
		t.Fatalf("unexpected walk result: rows=%v capped=%v", rows, capped)
	}
}

func TestArchivedDaysRangeExcludesPreviousDayAtMidnight(t *testing.T) {
	st := store.NewMem()
	for _, day := range []string{"2026-07-07", "2026-07-08", "2026-07-09"} {
		if err := st.MarkDayArchived(context.Background(), store.ArchivedDay{Day: day}); err != nil {
			t.Fatal(err)
		}
	}
	s := Server{store: st}
	f := SearchFilter{
		From: time.Date(2026, 7, 8, 0, 0, 0, 0, istLoc),
		To:   time.Date(2026, 7, 9, 23, 59, 0, 0, istLoc),
	}
	days := s.archivedDaysInRange(context.Background(), f)
	if len(days) != 2 {
		t.Fatalf("expected exactly two overlapping archive days, got %v", days)
	}
	got := map[string]bool{}
	for _, day := range days {
		got[day] = true
	}
	if !got["2026-07-08"] || !got["2026-07-09"] || got["2026-07-07"] {
		t.Fatalf("unexpected archive-day range: %v", days)
	}
}
