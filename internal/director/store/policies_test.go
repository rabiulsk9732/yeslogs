package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestPolicyStoreSnapshots(t *testing.T) {
	runPolicyStore(t, NewMem())
}
func TestMySQLPolicySnapshots(t *testing.T) {
	dsn := os.Getenv("YESLOGS_POLICY_TEST_DSN")
	if dsn == "" {
		t.Skip("isolated MariaDB DSN not configured")
	}
	// Never run this suite against an operational control-plane schema.
	if !strings.Contains(dsn, "/yeslogs_policy_test_") {
		t.Fatal("requires dedicated yeslogs_policy_test_ database")
	}
	st, e := OpenMySQL(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if e = st.Migrate(context.Background()); e != nil {
		t.Fatal(e)
	}
	runPolicyStore(t, st)
}
func runPolicyStore(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	a, _ := st.CreateISP(ctx, "policy-test-a")
	b, _ := st.CreateISP(ctx, "policy-test-b")
	p, e := st.SavePolicy(ctx, nil, &CapturePolicy{Name: "preset", ISPID: a.ID})
	if e != nil {
		t.Fatal(e)
	}
	fresh := p
	fresh.SkipZero = true
	updated, e := st.SavePolicy(ctx, &p, &fresh)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.SavePolicy(ctx, &p, &fresh); !errors.Is(e, ErrConflict) {
		t.Fatal("stale snapshot accepted", e)
	}
	if _, e = st.SavePolicy(ctx, &updated, &updated); e != nil {
		t.Fatal("no-op update failed", e)
	}
	if _, e = st.SavePolicy(ctx, nil, &CapturePolicy{Name: "PRESET"}); !errors.Is(e, ErrDuplicate) {
		t.Fatal("overlapping scope accepted", e)
	}
	if _, e = st.SavePolicy(ctx, nil, &CapturePolicy{Name: "preset", ISPID: b.ID}); e != nil {
		t.Fatal("separate tenants cannot reuse name", e)
	}
	d, e := st.CreateDevice(ctx, Device{Name: "edge", ISPID: a.ID, DeviceID: 1, ExporterIP: "192.0.2.10", Protocol: "auto", Profile: "generic", CapturePolicy: p.Name, Enabled: true})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = st.SavePolicy(ctx, &updated, nil); !errors.Is(e, ErrPolicyInUse) {
		t.Fatal("linked delete accepted", e)
	}
	changed := updated
	changed.Name = "renamed"
	if _, e = st.SavePolicy(ctx, &updated, &changed); !errors.Is(e, ErrPolicyInUse) {
		t.Fatal("linked rename accepted", e)
	}
	got, e := st.GetDevice(ctx, d.ID)
	if e != nil || got.SkipZero || !got.Enabled {
		t.Fatal("preset edit changed device", e)
	}
	if e = st.DeleteDevice(ctx, d.ID); e != nil {
		t.Fatal(e)
	}
	// Competing edits from the same version cannot both succeed.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			next := updated
			if n == 0 {
				next.SkipDNS = true
			} else {
				next.SkipPrivate = true
			}
			_, err := st.SavePolicy(ctx, &updated, &next)
			results <- err
		}(n)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatal("competing edits both succeeded")
	}
	list, e := st.ListPolicies(ctx, a.ID)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range list {
		if v.ID == p.ID {
			if _, e = st.SavePolicy(ctx, &v, nil); e != nil {
				t.Fatal(e)
			}
		}
	}
}
