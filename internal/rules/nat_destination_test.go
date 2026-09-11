package rules

import (
	"net"
	"testing"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

func TestDestinationTranslationSurvivesVolumeRules(t *testing.T) {
	for _, tc := range []struct {
		name, dst, postDst string
		dstPort, postPort  uint16
	}{
		{"DNAT with no byte counter", "203.0.113.176", "10.0.102.12", 42286, 42286},
		{"destination PAT", "203.0.113.176", "203.0.113.176", 42286, 52286},
		{"portless destination NAT", "203.0.113.176", "10.0.102.12", 0, 0},
		{"private destination mapping", "10.0.0.1", "10.0.102.12", 53, 53},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := normalizer.FlowRecord{
				SrcIP: net.ParseIP("10.0.1.1"), SrcPort: 53,
				DstIP: net.ParseIP(tc.dst), DstPort: tc.dstPort,
				NatDestIP: net.ParseIP(tc.postDst), NatDestPort: tc.postPort,
			}
			if skip, why := New(true, true, true).ShouldSkip(&r); skip {
				t.Fatalf("destination translation discarded as %q", why)
			}
		})
	}
}

func TestDestinationPortWithoutAddressDoesNotInventTranslation(t *testing.T) {
	r := normalizer.FlowRecord{DstIP: net.ParseIP("203.0.113.1"), DstPort: 1000, NatDestPort: 2000}
	if skip, why := New(false, false, false).ShouldSkip(&r); !skip || why != ReasonNoNAT {
		t.Fatalf("missing post-NAT IP: skip=%v reason=%q", skip, why)
	}
}

func TestUnchangedPostDestinationStillAllowsVolumeRules(t *testing.T) {
	r := normalizer.FlowRecord{DstIP: net.ParseIP("203.0.113.1"), DstPort: 1000,
		NatDestIP: net.ParseIP("203.0.113.1"), NatDestPort: 1000}
	if skip, why := New(true, true, true).ShouldSkip(&r); !skip || why != "zero_bytes" {
		t.Fatalf("unchanged destination tuple treated as translation: skip=%v reason=%q", skip, why)
	}
}
