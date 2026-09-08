package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestUserEditStores(t *testing.T) {
	t.Run("memory", func(t *testing.T) { userEditContract(t, NewMem()) })
	t.Run("mysql", func(t *testing.T) {
		dsn := os.Getenv("YESLOGS_USER_TEST_DSN")
		if dsn == "" {
			t.Skip("isolated user test DSN not provided")
		}
		if !strings.Contains(dsn, "/yeslogs_user_test_") {
			t.Fatal("use a dedicated yeslogs_user_test_ schema")
		}
		s, e := OpenMySQL(dsn)
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		if e = s.Migrate(context.Background()); e != nil {
			t.Fatal(e)
		}
		userEditContract(t, s)
	})
}
func userEditContract(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()
	isp, e := s.SaveISPAccount(ctx, ISP{Name: "User test", Username: "user.test", Email: "primary@example.invalid", Phone: "1234567890", Enabled: true}, "primary-hash")
	if e != nil {
		t.Fatal(e)
	}
	primary, e := s.GetUser(ctx, isp.AdminUserID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveUser(ctx, primary, nil); !errors.Is(e, ErrPrimaryUser) {
		t.Fatal("primary deletion accepted", e)
	}
	u, e := s.CreateUser(ctx, User{ISPID: isp.ID, Role: RoleISP, Email: "operator@example.invalid", PasswordHash: "old-hash"})
	if e != nil {
		t.Fatal(e)
	}
	next := u
	next.Email = "renamed@example.invalid"
	next.ISPID = 999
	next.Role = RoleDirector
	saved, e := s.SaveUser(ctx, u, &next)
	if e != nil {
		t.Fatal(e)
	}
	if saved.Email != next.Email || saved.ISPID != isp.ID || saved.Role != RoleISP || saved.PasswordHash != u.PasswordHash {
		t.Fatal("identity changed or email/password lost")
	}
	if _, e = s.SaveUser(ctx, u, &next); !errors.Is(e, ErrConflict) {
		t.Fatal("stale update accepted", e)
	}
	next = saved
	next.Email = primary.Email
	if _, e = s.SaveUser(ctx, saved, &next); !errors.Is(e, ErrDuplicate) {
		t.Fatal("duplicate email accepted", e)
	}
	next = saved
	next.PasswordHash = "replacement-hash"
	updated, e := s.SaveUser(ctx, saved, &next)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveUser(ctx, saved, nil); !errors.Is(e, ErrConflict) {
		t.Fatal("stale delete accepted", e)
	}
	if _, e = s.SaveUser(ctx, updated, nil); e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetUser(ctx, u.ID); !errors.Is(e, ErrNotFound) {
		t.Fatal("user not removed", e)
	}
	// Legacy tenants without a profile still protect their oldest ISP login.
	legacy, e := s.CreateISP(ctx, "Legacy user test")
	if e != nil {
		t.Fatal(e)
	}
	oldest, e := s.CreateUser(ctx, User{ISPID: legacy.ID, Role: RoleISP, Email: "legacy@example.invalid", PasswordHash: "legacy-hash"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SaveUser(ctx, oldest, nil); !errors.Is(e, ErrPrimaryUser) {
		t.Fatal("legacy primary deletion accepted", e)
	}
}
