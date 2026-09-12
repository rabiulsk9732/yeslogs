package crm

import (
	"context"
	"time"
)

// LookupStatus represents the outcome of a subscriber lookup.
type LookupStatus string

const (
	StatusMatched   LookupStatus = "matched"
	StatusNotFound  LookupStatus = "not_found"
	StatusAmbiguous LookupStatus = "ambiguous"
	StatusError     LookupStatus = "error"
	StatusTimeout   LookupStatus = "timeout"
)

// LookupRequest contains the necessary evidence to query a CRM or RADIUS system.
type LookupRequest struct {
	ReferenceCode string    `json:"referenceCode"`
	LocalIP       string    `json:"localIp"`
	EventTime     time.Time `json:"eventTime"`
	DeviceID      uint32    `json:"deviceId,omitempty"`
	NASIdentifier string    `json:"nasIdentifier,omitempty"`
	NASIPAddress  string    `json:"nasIpAddress,omitempty"`
}

// SubscriberInfo holds customer PII resolved from the CRM.
type SubscriberInfo struct {
	AccountID string `json:"accountId,omitempty"`
	Username  string `json:"username,omitempty"`
	Name      string `json:"name,omitempty"`
	Address   string `json:"address,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Email     string `json:"email,omitempty"`
}

// SessionInfo holds network session identifiers from RADIUS or BNG.
type SessionInfo struct {
	AcctSessionID    string `json:"acctSessionId,omitempty"`
	CallingStationID string `json:"callingStationId,omitempty"` // MAC address
	NASIPAddress     string `json:"nasIpAddress,omitempty"`
	NASPort          string `json:"nasPort,omitempty"`
	FramedIPAddress  string `json:"framedIpAddress,omitempty"`
}

// LookupResult is the enriched outcome for a single LookupRequest.
type LookupResult struct {
	ReferenceCode string         `json:"referenceCode"`
	Status        LookupStatus   `json:"status"`
	Subscriber    SubscriberInfo `json:"subscriber"`
	Session       SessionInfo    `json:"session"`
	ErrorMessage  string         `json:"errorMessage,omitempty"`
}

// Connector is the universal interface implemented by all CRM/RADIUS adapters.
type Connector interface {
	Lookup(ctx context.Context, lookups []LookupRequest) ([]LookupResult, error)
	Test(ctx context.Context, sample LookupRequest) (*LookupResult, error)
	Close() error
}
