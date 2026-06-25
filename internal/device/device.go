// Package device implements the exporter device registry: it maps an exporter's
// source IP to its ISP/device identity, vendor profile and per-device skip
// rules, and decides how to treat packets from exporters not in the registry.
// The registry is immutable once built and swapped atomically on reload.
package device

import (
	"fmt"
	"net"
	"sync/atomic"

	"github.com/natflow/natflow-dataplane/internal/rules"
)

// UnknownMode controls how packets from an unregistered exporter are handled.
type UnknownMode int

const (
	// ModeAllow decodes with the default isp_id/device_id (backward-compatible).
	ModeAllow UnknownMode = iota
	// ModeObserve decodes with isp_id=0/device_id=0 for visibility.
	ModeObserve
	// ModeReject drops the packet before decoding.
	ModeReject
)

func (m UnknownMode) String() string {
	switch m {
	case ModeObserve:
		return "observe"
	case ModeReject:
		return "reject"
	default:
		return "allow"
	}
}

// ParseUnknownMode parses a security.unknown_exporter_mode value. An empty
// string defaults to "allow" (preserves pre-registry behavior).
func ParseUnknownMode(s string) (UnknownMode, error) {
	switch s {
	case "", "allow":
		return ModeAllow, nil
	case "observe":
		return ModeObserve, nil
	case "reject":
		return ModeReject, nil
	default:
		return ModeAllow, fmt.Errorf("invalid unknown_exporter_mode %q (allow|observe|reject)", s)
	}
}

var validProfiles = map[string]bool{
	"": true, "mikrotik": true, "cisco": true, "juniper": true, "huawei": true, "generic": true,
}

var validProtocols = map[string]bool{
	"": true, "netflow5": true, "netflow9": true, "ipfix": true, "auto": true,
}

// RuleSpec is a per-device skip-rule override. Each nil field inherits the
// global rule; a set field overrides it.
type RuleSpec struct {
	SkipDNS              *bool `yaml:"skip_dns"`
	SkipPrivateToPrivate *bool `yaml:"skip_private_to_private"`
	SkipZeroBytes        *bool `yaml:"skip_zero_bytes"`
}

// Spec is one device entry as read from YAML.
type Spec struct {
	Name         string    `yaml:"name"`
	Enabled      bool      `yaml:"enabled"`
	ExporterIP   string    `yaml:"exporter_ip"`
	ISPID        uint32    `yaml:"isp_id"`
	DeviceID     uint32    `yaml:"device_id"`
	Protocol     string    `yaml:"protocol"`
	Profile      string    `yaml:"profile"`
	AllowedPorts []int     `yaml:"allowed_ports"`
	Rules        *RuleSpec `yaml:"rules"`
}

// Device is a resolved registry entry.
type Device struct {
	Name         string
	Enabled      bool
	ExporterIP   net.IP
	ISPID        uint32
	DeviceID     uint32
	Protocol     string
	Profile      string
	AllowedPorts []int
	Rules        rules.RuleSet // effective rules (global merged with overrides)
}

// Registry maps exporter IPs to devices.
type Registry struct {
	byIP map[string]*Device
}

// Lookup returns the device registered for ip, if any.
func (r *Registry) Lookup(ip net.IP) (*Device, bool) {
	if r == nil || ip == nil {
		return nil, false
	}
	d, ok := r.byIP[key(ip)]
	return d, ok
}

// Len returns the number of registered devices.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.byIP)
}

func key(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return string(v4)
	}
	return string(ip)
}

// Validate checks a set of device specs for the documented constraints without
// building a registry (used for early config validation).
func Validate(specs []Spec) error {
	seen := map[string]string{}
	for i, s := range specs {
		label := s.Name
		if label == "" {
			label = fmt.Sprintf("#%d", i)
		}
		ip := net.ParseIP(s.ExporterIP)
		if ip == nil {
			return fmt.Errorf("device %s: invalid exporter_ip %q", label, s.ExporterIP)
		}
		k := key(ip)
		if other, dup := seen[k]; dup {
			return fmt.Errorf("device %s: duplicate exporter_ip %s (also %s)", label, s.ExporterIP, other)
		}
		seen[k] = label
		if s.ISPID == 0 {
			return fmt.Errorf("device %s: isp_id must be > 0", label)
		}
		if s.DeviceID == 0 {
			return fmt.Errorf("device %s: device_id must be > 0", label)
		}
		if !validProtocol(s.Protocol) {
			return fmt.Errorf("device %s: invalid protocol %q", label, s.Protocol)
		}
		if !validProfile(s.Profile) {
			return fmt.Errorf("device %s: invalid profile %q", label, s.Profile)
		}
		for _, p := range s.AllowedPorts {
			if p < 1 || p > 65535 {
				return fmt.Errorf("device %s: allowed_port %d out of range 1-65535", label, p)
			}
		}
	}
	return nil
}

func validProfile(s string) bool  { return validProfiles[s] }
func validProtocol(s string) bool { return validProtocols[s] }

// Build validates specs and constructs a Registry. Each device's effective skip
// rules are the global rules with any per-device overrides applied.
func Build(specs []Spec, global rules.RuleSet) (*Registry, error) {
	if err := Validate(specs); err != nil {
		return nil, err
	}
	r := &Registry{byIP: make(map[string]*Device, len(specs))}
	for _, s := range specs {
		ip := net.ParseIP(s.ExporterIP)
		if v4 := ip.To4(); v4 != nil {
			ip = v4
		}
		eff := global
		if s.Rules != nil {
			if s.Rules.SkipDNS != nil {
				eff.SkipDNS = *s.Rules.SkipDNS
			}
			if s.Rules.SkipPrivateToPrivate != nil {
				eff.SkipPrivateToPrivate = *s.Rules.SkipPrivateToPrivate
			}
			if s.Rules.SkipZeroBytes != nil {
				eff.SkipZeroBytes = *s.Rules.SkipZeroBytes
			}
		}
		protocol := s.Protocol
		if protocol == "" {
			protocol = "auto"
		}
		profile := s.Profile
		if profile == "" {
			profile = "generic"
		}
		r.byIP[key(ip)] = &Device{
			Name:         s.Name,
			Enabled:      s.Enabled,
			ExporterIP:   ip,
			ISPID:        s.ISPID,
			DeviceID:     s.DeviceID,
			Protocol:     protocol,
			Profile:      profile,
			AllowedPorts: s.AllowedPorts,
			Rules:        eff,
		}
	}
	return r, nil
}

// Store holds the current Registry behind an atomic pointer for lock-free reads.
type Store struct {
	v atomic.Pointer[Registry]
}

// NewStore creates a Store seeded with r.
func NewStore(r *Registry) *Store {
	s := &Store{}
	s.v.Store(r)
	return s
}

// Load returns the current Registry.
func (s *Store) Load() *Registry { return s.v.Load() }

// Store atomically replaces the current Registry.
func (s *Store) Store(r *Registry) { s.v.Store(r) }
