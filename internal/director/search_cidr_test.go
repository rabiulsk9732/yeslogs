package director

import (
	"strings"
	"testing"
)

func TestHotWhere_CIDR_Subnet(t *testing.T) {
	f := SearchFilter{
		ISPID:    1,
		PublicIP: "103.204.1.0/24",
	}

	if !f.HasSelector() {
		t.Fatal("CIDR filter must be recognized as a valid selector")
	}

	where, args, ok := hotWhere(f)
	if !ok {
		t.Fatal("hotWhere rejected CIDR filter")
	}

	if !strings.Contains(where, "nat_public_ip >= toIPv4(?)") || !strings.Contains(where, "nat_public_ip <= toIPv4(?)") {
		t.Fatalf("expected CIDR range in where clause, got: %s", where)
	}

	foundStart, foundEnd := false, false
	for _, a := range args {
		if s, ok := a.(string); ok {
			if s == "103.204.1.0" {
				foundStart = true
			}
			if s == "103.204.1.255" {
				foundEnd = true
			}
		}
	}
	if !foundStart || !foundEnd {
		t.Fatalf("expected args to contain 103.204.1.0 and 103.204.1.255, got: %v", args)
	}
}

func TestHotWhere_Multi_IP_List(t *testing.T) {
	f := SearchFilter{
		ISPID:    1,
		PublicIP: "103.204.1.10, 103.204.1.20",
	}

	if !f.HasSelector() {
		t.Fatal("IP list filter must be recognized as a valid selector")
	}

	where, args, ok := hotWhere(f)
	if !ok {
		t.Fatal("hotWhere rejected multi-IP filter")
	}

	if !strings.Contains(where, "nat_public_ip IN (toIPv4(?), toIPv4(?))") {
		t.Fatalf("expected IN clause for multi-IP, got: %s", where)
	}

	if len(args) < 2 {
		t.Fatalf("expected at least 2 args, got: %v", args)
	}
}

func TestSearchIPValidationRejectsMalformedAndMixedFamilyLists(t *testing.T) {
	for _, raw := range []string{"103.204.1.10,2001:db8::1", "103.204.1.999", "2001:db8::/129", "103.204.1.0/33"} {
		if err := validateSearchIPs(SearchFilter{PublicIP: raw}); err == nil {
			t.Fatalf("expected invalid filter %q to be rejected", raw)
		}
	}
	for _, raw := range []string{"103.204.1.10,103.204.1.20", "2001:db8::1,2001:db8::2", "2001:db8::/64"} {
		if err := validateSearchIPs(SearchFilter{PublicIP: raw}); err != nil {
			t.Fatalf("expected valid filter %q: %v", raw, err)
		}
	}
}

func TestHotWhereIPv6UsesShadowColumnsOnlyAfterMigration(t *testing.T) {
	f := SearchFilter{ISPID: 1, PublicIP: "2001:db8::10", ipv6Available: true}
	where, args, ok := hotWhere(f)
	if !ok || !strings.Contains(where, "nat_public_ip_v6 = toIPv6(?)") || len(args) < 1 || args[0] != f.PublicIP {
		t.Fatalf("expected migrated IPv6 predicate, got %q %#v", where, args)
	}
	f.ipv6Available = false
	where, args, ok = hotWhere(f)
	if !ok || !strings.Contains(where, "0") || len(args) != 1 || args[0] != f.ISPID {
		t.Fatalf("pre-migration IPv6 query must safely return no matches, got %q %#v", where, args)
	}
}

func TestHotWhere_Port_Range(t *testing.T) {
	f := SearchFilter{
		ISPID:     1,
		PublicIP:  "103.204.1.10",
		PortRange: "20000-25000",
	}

	where, args, ok := hotWhere(f)
	if !ok {
		t.Fatal("hotWhere rejected port range filter")
	}

	if !strings.Contains(where, "nat_public_port >= ?") || !strings.Contains(where, "nat_public_port <= ?") {
		t.Fatalf("expected port range in where clause, got: %s", where)
	}

	foundStart, foundEnd := false, false
	for _, a := range args {
		if u, ok := a.(uint16); ok {
			if u == 20000 {
				foundStart = true
			}
			if u == 25000 {
				foundEnd = true
			}
		}
	}
	if !foundStart || !foundEnd {
		t.Fatalf("expected port args 20000 and 25000, got: %v", args)
	}
}
