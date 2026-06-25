package store

import (
	"context"
	"errors"
	"testing"
)

func TestMemISPAndDevice(t *testing.T) {
	ctx := context.Background()
	m := NewMem()

	isp, err := m.CreateISP(ctx, "Acme")
	if err != nil || isp.ID == 0 {
		t.Fatalf("create isp: %v id=%d", err, isp.ID)
	}
	if _, err := m.CreateISP(ctx, "Acme"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("dup isp name should be ErrDuplicate, got %v", err)
	}

	d := Device{ISPID: isp.ID, Name: "r1", ExporterIP: "203.0.113.1", DeviceID: 5, Protocol: "auto", Profile: "generic", Enabled: true, SkipDNS: true}
	created, err := m.CreateDevice(ctx, d)
	if err != nil || created.ID == 0 {
		t.Fatalf("create device: %v", err)
	}
	if _, err := m.CreateDevice(ctx, d); !errors.Is(err, ErrDuplicate) {
		t.Errorf("dup exporter_ip should be ErrDuplicate, got %v", err)
	}
	if _, err := m.GetDevice(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing device should be ErrNotFound, got %v", err)
	}

	devs, _ := m.ListDevices(ctx, isp.ID)
	if len(devs) != 1 {
		t.Fatalf("list scoped: %d", len(devs))
	}
	all, _ := m.ListDevices(ctx, 0)
	if len(all) != 1 {
		t.Fatalf("list all: %d", len(all))
	}

	created.Enabled = false
	if err := m.UpdateDevice(ctx, created); err != nil {
		t.Fatal(err)
	}
	got, _ := m.GetDevice(ctx, created.ID)
	if got.Enabled {
		t.Error("update enabled=false not applied")
	}
	if err := m.DeleteDevice(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetDevice(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Error("device should be gone")
	}
}

func TestMemUsersAndAgents(t *testing.T) {
	ctx := context.Background()
	m := NewMem()
	if n, _ := m.CountUsers(ctx); n != 0 {
		t.Fatal("expected 0 users")
	}
	_, err := m.CreateUser(ctx, User{Email: "A@X.com", PasswordHash: "h", Role: RoleDirector})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetUserByEmail(ctx, "a@x.com"); err != nil {
		t.Errorf("email lookup should be case-insensitive: %v", err)
	}
	if _, err := m.CreateUser(ctx, User{Email: "a@x.com", PasswordHash: "h", Role: RoleISP}); !errors.Is(err, ErrDuplicate) {
		t.Error("dup email should fail")
	}

	a, err := m.CreateAgent(ctx, "dp1", "hash123")
	if err != nil || a.ID == 0 {
		t.Fatalf("create agent: %v", err)
	}
	if _, err := m.GetAgentByToken(ctx, "hash123"); err != nil {
		t.Errorf("agent lookup: %v", err)
	}
	if _, err := m.GetAgentByToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Error("bad token should be ErrNotFound")
	}
}
