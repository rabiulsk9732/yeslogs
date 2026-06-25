package device

import (
	"net"
	"sync"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/rules"
)

func boolp(b bool) *bool { return &b }

func TestDuplicateExporterRejected(t *testing.T) {
	specs := []Spec{
		{Name: "a", ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 1},
		{Name: "b", ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 2},
	}
	if _, err := Build(specs, rules.RuleSet{}); err == nil {
		t.Fatal("want error for duplicate exporter_ip")
	}
}

func TestValidateConstraints(t *testing.T) {
	cases := []struct {
		name string
		s    Spec
	}{
		{"bad ip", Spec{ExporterIP: "not-an-ip", ISPID: 1, DeviceID: 1}},
		{"isp 0", Spec{ExporterIP: "203.0.113.1", ISPID: 0, DeviceID: 1}},
		{"device 0", Spec{ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 0}},
		{"bad protocol", Spec{ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 1, Protocol: "sflow"}},
		{"bad profile", Spec{ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 1, Profile: "ubiquiti"}},
		{"bad port", Spec{ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 1, AllowedPorts: []int{70000}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := Validate([]Spec{c.s}); err == nil {
				t.Errorf("want validation error for %s", c.name)
			}
		})
	}
}

func TestValidAcceptedAndDefaults(t *testing.T) {
	reg, err := Build([]Spec{
		{Name: "ok", Enabled: true, ExporterIP: "203.0.113.7", ISPID: 5, DeviceID: 9},
	}, rules.RuleSet{})
	if err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	d, ok := reg.Lookup(net.ParseIP("203.0.113.7"))
	if !ok {
		t.Fatal("device not found")
	}
	if d.Protocol != "auto" || d.Profile != "generic" {
		t.Errorf("defaults not applied: protocol=%q profile=%q", d.Protocol, d.Profile)
	}
	if d.ISPID != 5 || d.DeviceID != 9 {
		t.Errorf("identity = %d/%d", d.ISPID, d.DeviceID)
	}
}

func TestPerDeviceRuleOverrideMerge(t *testing.T) {
	global := rules.RuleSet{SkipDNS: true, SkipPrivateToPrivate: true, SkipZeroBytes: true}
	reg, err := Build([]Spec{
		{
			Name: "d", Enabled: true, ExporterIP: "203.0.113.7", ISPID: 1, DeviceID: 1,
			Rules: &RuleSpec{SkipDNS: boolp(false)}, // override only skip_dns
		},
	}, global)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := reg.Lookup(net.ParseIP("203.0.113.7"))
	if d.Rules.SkipDNS {
		t.Error("device skip_dns override (false) not applied")
	}
	if !d.Rules.SkipPrivateToPrivate || !d.Rules.SkipZeroBytes {
		t.Error("non-overridden rules should inherit global (true)")
	}
}

func TestParseUnknownMode(t *testing.T) {
	for in, want := range map[string]UnknownMode{"": ModeAllow, "allow": ModeAllow, "observe": ModeObserve, "reject": ModeReject} {
		if got, err := ParseUnknownMode(in); err != nil || got != want {
			t.Errorf("ParseUnknownMode(%q)=%v,%v want %v", in, got, err, want)
		}
	}
	if _, err := ParseUnknownMode("nope"); err == nil {
		t.Error("want error for invalid mode")
	}
}

func TestStoreReloadSafely(t *testing.T) {
	reg1, _ := Build([]Spec{{Name: "a", Enabled: true, ExporterIP: "203.0.113.1", ISPID: 1, DeviceID: 1}}, rules.RuleSet{})
	reg2, _ := Build([]Spec{{Name: "b", Enabled: true, ExporterIP: "203.0.113.2", ISPID: 1, DeviceID: 2}}, rules.RuleSet{})
	s := NewStore(reg1)

	if _, ok := s.Load().Lookup(net.ParseIP("203.0.113.1")); !ok {
		t.Fatal("reg1 device A missing")
	}

	// Concurrent lookups during a swap must be race-free (run under -race).
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ipA, ipB := net.ParseIP("203.0.113.1"), net.ParseIP("203.0.113.2")
			for {
				select {
				case <-stop:
					return
				default:
					s.Load().Lookup(ipA)
					s.Load().Lookup(ipB)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			s.Store(reg2)
		} else {
			s.Store(reg1)
		}
	}
	close(stop)
	wg.Wait()

	s.Store(reg2)
	if _, ok := s.Load().Lookup(net.ParseIP("203.0.113.2")); !ok {
		t.Error("after reload, reg2 device B missing")
	}
	if _, ok := s.Load().Lookup(net.ParseIP("203.0.113.1")); ok {
		t.Error("after reload, reg1 device A should be gone")
	}
}
