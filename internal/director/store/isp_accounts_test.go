package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestISPAccountStores(t *testing.T) {
	t.Run("memory", func(t *testing.T) { ispAccountContract(t, NewMem()) })
	t.Run("mysql", func(t *testing.T) {
		dsn := os.Getenv("YESLOGS_TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("isolated MySQL DSN not provided")
		}
		s, e := OpenMySQL(dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		if e = s.Migrate(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e = s.Migrate(context.Background()); e != nil {
			t.Fatal(e)
		}
		ispAccountContract(t, s)
	})
}
func ispAccountContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	primary := ISP{Name: "Primary", Username: "primary", Email: "primary@example.invalid", Phone: "+91 9876543210", Enabled: true}
	a, e := s.SaveISPAccount(ctx, primary, "hash-a")
	if e != nil {
		t.Fatal(e)
	}
	if a.Version != 1 || a.AdminUserID == 0 {
		t.Fatalf("invalid created profile: %+v", a)
	}
	if u, e := s.GetUserByLogin(ctx, "PRIMARY"); e != nil || u.ISPID != a.ID || u.PasswordHash != "hash-a" {
		t.Fatalf("username lookup: %+v %v", u, e)
	}
	for _, field := range []string{"name", "username", "email"} {
		b := ISP{Name: "Unique", Username: "unique", Email: "unique@example.invalid", Phone: "1234567890"}
		switch field {
		case "name":
			b.Name = a.Name
		case "username":
			b.Username = a.Username
		case "email":
			b.Email = a.Email
		}
		if _, e = s.SaveISPAccount(ctx, b, "hash-b"); !errors.Is(e, ErrDuplicate) {
			t.Fatalf("duplicate %s: %v", field, e)
		}
		isps, _ := s.ListISPs(ctx)
		users, _ := s.ListUsers(ctx, 0)
		if len(isps) != 1 || len(users) != 1 {
			t.Fatal("failed onboarding left an orphan tenant or user")
		}
	}
	other, e := s.SaveISPAccount(ctx, ISP{Name: "Other", Username: "other", Email: "other@example.invalid", Phone: "1234567890"}, "hash-b")
	if e != nil {
		t.Fatal(e)
	}
	stale := a
	a.Name = "Renamed"
	a.Email = "new@example.invalid"
	a.Enabled = false
	saved, e := s.SaveISPAccount(ctx, a, "")
	if e != nil {
		t.Fatal(e)
	}
	if saved.Version != 2 {
		t.Fatal("version did not advance")
	}
	if _, e = s.GetUserByEmail(ctx, "primary@example.invalid"); !errors.Is(e, ErrNotFound) {
		t.Fatal("old email still resolves")
	}
	if u, e := s.GetUserByLogin(ctx, "primary"); e != nil || u.PasswordHash != "hash-a" || u.Email != "new@example.invalid" {
		t.Fatal("edit changed the password or lost login")
	}
	if _, e = s.SaveISPAccount(ctx, stale, "changed"); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale edit accepted: %v", e)
	}
	attempt := saved
	attempt.Email = other.Email
	attempt.Name = "Must roll back"
	if _, e = s.SaveISPAccount(ctx, attempt, "changed"); !errors.Is(e, ErrDuplicate) {
		t.Fatal(e)
	}
	a, e = s.GetISP(ctx, saved.ID)
	if e != nil || a.Name != "Renamed" || a.Version != 2 {
		t.Fatal("failed edit partially updated tenant")
	}
	d, e := s.CreateDevice(ctx, Device{ISPID: a.ID, Name: "edge", ExporterIP: "192.0.2.251", Protocol: "auto", Profile: "generic"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteISPAccount(ctx, a.ID, a.Version); !errors.Is(e, ErrInUse) {
		t.Fatalf("deleted ISP with device: %v", e)
	}
	if e = s.DeleteDevice(ctx, d.ID); e != nil {
		t.Fatal(e)
	}
	p, e := s.CreatePolicy(ctx, CapturePolicy{ISPID: a.ID, Name: "private"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteISPAccount(ctx, a.ID, a.Version); !errors.Is(e, ErrInUse) {
		t.Fatalf("deleted ISP with policy: %v", e)
	}
	if e = s.DeletePolicy(ctx, p.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.LogQuery(ctx, QueryAudit{ISPID: a.ID, UserEmail: "operator@example.invalid", CaseRef: "retained"}); e != nil {
		t.Fatal(e)
	}
	if e = s.DeleteISPAccount(ctx, a.ID, a.Version); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetISP(ctx, a.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("deleted ISP still exists")
	}
	if _, e = s.GetUserByLogin(ctx, "primary"); !errors.Is(e, ErrNotFound) {
		t.Fatal("deleted login still exists")
	}
	q, e := s.ListQueries(ctx, a.ID, 10)
	if e != nil || len(q) != 1 {
		t.Fatal("delete damaged audit history")
	}
	if u, e := s.GetUserByLogin(ctx, "other"); e != nil || u.ISPID != other.ID {
		t.Fatal("delete crossed tenant scope")
	}
	legacy, e := s.CreateISP(ctx, "Legacy")
	if e != nil {
		t.Fatal(e)
	}
	u, e := s.CreateUser(ctx, User{ISPID: legacy.ID, Email: "legacy@example.invalid", PasswordHash: "old-hash", Role: RoleISP})
	if e != nil {
		t.Fatal(e)
	}
	legacy, e = s.GetISP(ctx, legacy.ID)
	if e != nil || legacy.AdminUserID != u.ID || legacy.Email != u.Email {
		t.Fatal("legacy primary not resolved")
	}
	legacy.Username = "legacy"
	legacy.Phone = "1234567890"
	legacy, e = s.SaveISPAccount(ctx, legacy, "")
	if e != nil {
		t.Fatal(e)
	}
	if got, e := s.GetUserByLogin(ctx, "legacy"); e != nil || got.PasswordHash != "old-hash" {
		t.Fatal("legacy upgrade changed password")
	}
	if e = s.DeleteISPAccount(ctx, legacy.ID, legacy.Version); !errors.Is(e, ErrConflict) {
		t.Fatal("active tenant deletion accepted")
	}
}
