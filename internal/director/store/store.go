// Package store defines the Director control-plane data model and the storage
// interface backing it (MariaDB/MySQL in production, an in-memory fake in tests).
package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors.
var (
	ErrNotFound  = errors.New("not found")
	ErrDuplicate = errors.New("duplicate")
)

// Role identifies a user's privilege level.
type Role string

const (
	// RoleDirector is the software owner (Sayra) — sees and manages everything.
	RoleDirector Role = "director"
	// RoleISP is a tenant user — scoped to a single ISP.
	RoleISP Role = "isp"
)

// ISP is a tenant (a customer ISP that produces flow logs).
type ISP struct {
	ID        uint32 // also the flow isp_id stamped on records
	Name      string
	Enabled   bool
	CreatedAt time.Time
}

// User is a login. Director users have ISPID == 0; ISP users are scoped to one.
type User struct {
	ID           int64
	ISPID        uint32
	Email        string
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
}

// Device is an exporter belonging to an ISP. It maps to one device-registry entry
// pushed down to the dataplane.
type Device struct {
	ID            int64
	ISPID         uint32
	Name          string
	ExporterIP    string
	DeviceID      uint32
	Protocol      string // netflow5|netflow9|ipfix|auto
	Profile       string // mikrotik|cisco|juniper|huawei|generic
	CapturePolicy string // optional named capture policy this device uses
	Enabled       bool
	SkipDNS       bool
	SkipPrivate   bool
	SkipZero      bool
	UpdatedAt     time.Time
}

// CapturePolicy is a reusable named skip-rule preset for exporter devices.
type CapturePolicy struct {
	ID          int64
	ISPID       uint32 // 0 = global (director)
	Name        string
	SkipDNS     bool
	SkipPrivate bool
	SkipZero    bool
	CreatedAt   time.Time
}

// Agent is a dataplane collector that pulls its config from the Director.
type Agent struct {
	ID        int64
	Name      string
	TokenHash string
	LastSeen  time.Time
	CreatedAt time.Time
}

// QueryAudit is an immutable record of a lawful IPDR lookup (who searched what,
// when, and how many records matched) — required for lawful-intercept audit.
type QueryAudit struct {
	ID          int64
	UserEmail   string
	ISPID       uint32
	QueryIP     string
	QueryPort   int    // 0 = any
	QueryProto  string // "" = any
	FromTS      time.Time
	ToTS        time.Time
	ResultCount int
	CaseRef     string
	CreatedAt   time.Time
}

// Store is the Director's persistence interface.
type Store interface {
	Migrate(ctx context.Context) error

	CreateISP(ctx context.Context, name string) (ISP, error)
	ListISPs(ctx context.Context) ([]ISP, error)
	GetISP(ctx context.Context, id uint32) (ISP, error)
	SetISPEnabled(ctx context.Context, id uint32, enabled bool) error

	CreateUser(ctx context.Context, u User) (User, error)
	GetUserByEmail(ctx context.Context, email string) (User, error)
	CountUsers(ctx context.Context) (int, error)

	CreateDevice(ctx context.Context, d Device) (Device, error)
	// ListDevices returns devices for ispID, or all devices when ispID == 0.
	ListDevices(ctx context.Context, ispID uint32) ([]Device, error)
	GetDevice(ctx context.Context, id int64) (Device, error)
	UpdateDevice(ctx context.Context, d Device) error
	DeleteDevice(ctx context.Context, id int64) error

	CreateAgent(ctx context.Context, name, tokenHash string) (Agent, error)
	GetAgentByToken(ctx context.Context, tokenHash string) (Agent, error)
	TouchAgent(ctx context.Context, id int64, t time.Time) error

	// LogQuery records a lawful IPDR lookup; ListQueries returns recent audits
	// (ispID 0 = all, director only).
	LogQuery(ctx context.Context, q QueryAudit) (int64, error)
	ListQueries(ctx context.Context, ispID uint32, limit int) ([]QueryAudit, error)

	// Settings are JSON blobs keyed by section (dataplane, skiprules, retention,
	// s3) — the editable, DB-backed source of truth (YAML is only bootstrap).
	GetSettings(ctx context.Context) (map[string]string, error)
	PutSetting(ctx context.Context, section, jsonData string) error

	// Capture policies (reusable skip-rule presets; ispID 0 = global/all).
	CreatePolicy(ctx context.Context, p CapturePolicy) (CapturePolicy, error)
	ListPolicies(ctx context.Context, ispID uint32) ([]CapturePolicy, error)
	DeletePolicy(ctx context.Context, id int64) error

	Close() error
}
