package pipeline_test

import (
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/rules"
)

func bptr(b bool) *bool { return &b }

// staticDecoder returns a fixed set of flows regardless of payload.
type staticDecoder struct {
	flows []decoder.Flow
}

func (staticDecoder) Kind() string { return "netflow5" }
func (d staticDecoder) Decode(dst []decoder.Flow, _ []byte, exporter net.IP) ([]decoder.Flow, error) {
	for _, f := range d.flows {
		f.ExporterIP = exporter
		dst = append(dst, f)
	}
	return dst, nil
}

func normalFlow() decoder.Flow {
	return decoder.Flow{SrcIP: net.IPv4(10, 0, 0, 5), DstIP: net.IPv4(1, 1, 1, 1), SrcPort: 40000, DstPort: 443, Protocol: 6, Bytes: 1500, Packets: 10}
}
func dnsFlow() decoder.Flow {
	return decoder.Flow{SrcIP: net.IPv4(10, 0, 0, 5), DstIP: net.IPv4(8, 8, 8, 8), SrcPort: 40001, DstPort: 53, Protocol: 17, Bytes: 120, Packets: 2}
}

func buildPipe(live *config.Store, devs *device.Store, defISP, defDev uint32, flows ...decoder.Flow) (*pipeline.Pipeline, *captureWriter, *metrics.Metrics) {
	m := metrics.New()
	w := &captureWriter{}
	log := slog.New(slog.NewJSONHandler(io.Discard, nil))
	p := pipeline.New(staticDecoder{flows: flows}, normalizer.New(), live, devs, defISP, defDev, w, m, log)
	return p, w, m
}

func devStore(t *testing.T, global rules.RuleSet, specs ...device.Spec) *device.Store {
	t.Helper()
	r, err := device.Build(specs, global)
	if err != nil {
		t.Fatalf("device.Build: %v", err)
	}
	return device.NewStore(r)
}

func TestMatchedExporterSetsIdentity(t *testing.T) {
	devs := devStore(t, rules.RuleSet{}, device.Spec{Name: "r1", Enabled: true, ExporterIP: "203.0.113.5", ISPID: 7, DeviceID: 9})
	live := config.NewStore(config.Live{UnknownMode: device.ModeReject})
	p, w, m := buildPipe(live, devs, 1, 2, normalFlow())

	p.HandlePacket([]byte{0}, net.ParseIP("203.0.113.5"))

	recs := w.records()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].ISPID != 7 || recs[0].DeviceID != 9 {
		t.Errorf("identity = %d/%d, want 7/9", recs[0].ISPID, recs[0].DeviceID)
	}
	if v := testutil.ToFloat64(m.DeviceMatchedPackets); v != 1 {
		t.Errorf("device_matched_packets_total = %v", v)
	}
}

func TestUnknownRejectMode(t *testing.T) {
	live := config.NewStore(config.Live{UnknownMode: device.ModeReject})
	p, w, m := buildPipe(live, emptyDevices(), 1, 2, normalFlow())

	p.HandlePacket([]byte{0}, net.ParseIP("198.51.100.9"))

	if len(w.records()) != 0 {
		t.Fatalf("reject mode must drop, got %d records", len(w.records()))
	}
	if v := testutil.ToFloat64(m.UnknownExporterPackets); v != 1 {
		t.Errorf("unknown_exporter_packets_total = %v", v)
	}
	if v := testutil.ToFloat64(m.DeviceRejectedPackets); v != 1 {
		t.Errorf("device_rejected_packets_total = %v", v)
	}
}

func TestUnknownObserveMode(t *testing.T) {
	live := config.NewStore(config.Live{UnknownMode: device.ModeObserve})
	p, w, m := buildPipe(live, emptyDevices(), 5, 6, normalFlow())

	p.HandlePacket([]byte{0}, net.ParseIP("198.51.100.9"))

	recs := w.records()
	if len(recs) != 1 {
		t.Fatalf("observe mode should decode, got %d records", len(recs))
	}
	if recs[0].ISPID != 0 || recs[0].DeviceID != 0 {
		t.Errorf("observe identity = %d/%d, want 0/0", recs[0].ISPID, recs[0].DeviceID)
	}
	if v := testutil.ToFloat64(m.UnknownExporterFlows); v != 1 {
		t.Errorf("unknown_exporter_flows_total = %v", v)
	}
}

func TestUnknownAllowMode(t *testing.T) {
	live := config.NewStore(config.Live{UnknownMode: device.ModeAllow})
	p, w, _ := buildPipe(live, emptyDevices(), 5, 6, normalFlow())

	p.HandlePacket([]byte{0}, net.ParseIP("198.51.100.9"))

	recs := w.records()
	if len(recs) != 1 || recs[0].ISPID != 5 || recs[0].DeviceID != 6 {
		t.Fatalf("allow mode should use defaults 5/6, got %d recs id=%d/%d", len(recs), recs[0].ISPID, recs[0].DeviceID)
	}
}

func TestDisabledDeviceRejected(t *testing.T) {
	devs := devStore(t, rules.RuleSet{}, device.Spec{Name: "off", Enabled: false, ExporterIP: "203.0.113.5", ISPID: 7, DeviceID: 9})
	live := config.NewStore(config.Live{UnknownMode: device.ModeAllow})
	p, w, m := buildPipe(live, devs, 1, 2, normalFlow())

	p.HandlePacket([]byte{0}, net.ParseIP("203.0.113.5"))

	if len(w.records()) != 0 {
		t.Fatalf("disabled device must be rejected, got %d records", len(w.records()))
	}
	if v := testutil.ToFloat64(m.DeviceRejectedPackets); v != 1 {
		t.Errorf("device_rejected_packets_total = %v", v)
	}
	if v := testutil.ToFloat64(m.DeviceMatchedPackets); v != 0 {
		t.Errorf("device_matched_packets_total = %v, want 0", v)
	}
}

func TestPerDeviceSkipDNSOverridesGlobal(t *testing.T) {
	global := rules.RuleSet{} // global: skip nothing (DNS kept)
	devs := devStore(t, global, device.Spec{
		Name: "d", Enabled: true, ExporterIP: "203.0.113.5", ISPID: 7, DeviceID: 9,
		Rules: &device.RuleSpec{SkipDNS: bptr(true)}, // this device skips DNS
	})
	live := config.NewStore(config.Live{Rules: global, UnknownMode: device.ModeAllow})

	// Matched device -> per-device skip_dns=true drops the DNS flow.
	p, w, m := buildPipe(live, devs, 1, 2, dnsFlow())
	p.HandlePacket([]byte{0}, net.ParseIP("203.0.113.5"))
	if len(w.records()) != 0 {
		t.Fatalf("device skip_dns should drop DNS flow, got %d records", len(w.records()))
	}
	if v := testutil.ToFloat64(m.DeviceRuleSkipped); v != 1 {
		t.Errorf("device_rule_skipped_total = %v, want 1", v)
	}

	// Unknown exporter (global rules, skip_dns=false) -> DNS flow kept.
	p2, w2, _ := buildPipe(live, devs, 1, 2, dnsFlow())
	p2.HandlePacket([]byte{0}, net.ParseIP("198.51.100.9"))
	if len(w2.records()) != 1 {
		t.Fatalf("global rules keep DNS for unknown exporter, got %d records", len(w2.records()))
	}
}
