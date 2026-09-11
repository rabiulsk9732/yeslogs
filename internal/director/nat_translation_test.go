package director

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/director/store"
)

func observedDestinationRecord() natRecord {
	// Sanitized destination-translation shape independently observed on NetFlow v9.
	return natRecord{PrivIP: "192.0.2.44", PrivPort: 443, DstIP: "203.0.113.176", DstPort: 42286,
		PubIP: "192.0.2.44", PubPort: 443, PostDstIP: "10.0.0.12", PostDstPort: 42286}
}

func TestColdNATSchemaPreservesStoredTenantAndUnknownDefaults(t *testing.T) {
	// Real S3 paths contain isp_id=<tenant>; Hive inference steals the stored
	// column from the SELECT/LIMIT BY block on ClickHouse 26.7.
	for _, setting := range []string{"use_hive_partitioning = 0", "input_format_parquet_allow_missing_columns = 1", "input_format_with_names_use_header = 1"} {
		if !strings.Contains(coldReadSettings, setting) {
			t.Errorf("cold compatibility setting missing: %s", setting)
		}
	}
}

func TestNATTranslationPreservesDirectionAndMissingEvidence(t *testing.T) {
	for _, tc := range []struct {
		name       string
		edit       func(*natRecord)
		status, ip string
		port       int
		unchanged  bool
	}{
		{"observed destination", func(*natRecord) {}, "destination", "", 0, false},
		{"historical destination missing", func(r *natRecord) { r.PostDstIP = ""; r.PostDstPort = 0 }, "unknown", "", 0, false},
		{"same IP changed destination port", func(r *natRecord) { r.PostDstIP = r.DstIP; r.PostDstPort = 1234 }, "destination", "", 0, false},
		{"source port only", func(r *natRecord) { r.PubPort = 0; r.PostDstIP = r.DstIP; r.PostDstPort = r.DstPort }, "source", "192.0.2.44", 0, false},
		{"both sides", func(r *natRecord) { r.PubIP = "203.0.113.1" }, "both", "203.0.113.1", 443, false},
		{"both unchanged", func(r *natRecord) { r.PostDstIP = r.DstIP; r.PostDstPort = r.DstPort }, "none", "", 0, true},
		{"zero sentinel old schema", func(r *natRecord) { r.PostDstIP = "0.0.0.0"; r.PostDstPort = 0 }, "unknown", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := observedDestinationRecord()
			tc.edit(&r)
			before := r
			r.setNATTranslation()
			if r.Translation != tc.status || r.NatIP != tc.ip || r.NatPort != tc.port || r.Untranslated != tc.unchanged {
				t.Fatalf("unexpected mapping: %+v", r)
			}
			if r.PrivIP != before.PrivIP || r.PrivPort != before.PrivPort || r.DstIP != before.DstIP || r.DstPort != before.DstPort {
				t.Fatal("raw direction was changed")
			}
			if r.PostSrcIP != before.PubIP || r.PostSrcPort != before.PubPort {
				t.Fatal("raw post-source tuple lost")
			}
		})
	}
}

func TestNATReportUsesExactEightFieldsAndDoesNotInventMappingOrEndTime(t *testing.T) {
	r := observedDestinationRecord()
	r.Time, r.EndTime = "2026-09-11 12:00:01", "2026-09-11 12:01:22"
	cells := rowCells(reportMeta{}, r)
	want := []string{"09:11:2026 & 12:00:01", "09:11:2026 & 12:01:22", "192.0.2.44", "443", "", "", "203.0.113.176", "42286"}
	if !reflect.DeepEqual(cells, want) {
		t.Fatalf("wrong raw/mapping report: %v", cells)
	}
	r.PubIP, r.PubPort = "203.0.113.10", 12345
	cells = rowCells(reportMeta{}, r)
	if cells[4] != r.PubIP || cells[5] != "12345" {
		t.Fatalf("confirmed source mapping lost: %v", cells)
	}
	r.PrivPort, r.EndTime = 0, ""
	if cells := rowCells(reportMeta{}, r); cells[1] != "" || cells[3] != "0" {
		t.Fatalf("invented end time or lost zero port: %v", cells)
	}
	for _, crm := range []bool{false, true} {
		m := reportMeta{CRM: crmEnrichmentSummary{Enabled: crm}}
		cols, _ := reportColumns(m)
		if len(cols) != 8 || len(rowCells(m, r)) != 8 {
			t.Fatal("report shape changed with CRM")
		}
		var out bytes.Buffer
		if err := writeCSV(&out, m, []natRecord{r}); err != nil {
			t.Fatal(err)
		}
		reader := csv.NewReader(&out)
		reader.FieldsPerRecord = -1
		data, err := reader.ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 2 || !reflect.DeepEqual(data[0], baseReportCols) || len(data[1]) != 8 {
			t.Fatalf("wrong CSV table: %v", data)
		}
		out.Reset()
		if err := writePDF(&out, m, []natRecord{r}); err != nil {
			t.Fatal(err)
		}
		if out.Len() < 500 {
			t.Fatal("empty PDF")
		}
	}
}

func TestExporterIdentityDoesNotOverwriteSharedDeviceID(t *testing.T) {
	s, st := testServer(t)
	ctx := context.Background()
	isp, _ := st.CreateISP(ctx, "Xcess")
	other, _ := st.CreateISP(ctx, "Other")
	for _, d := range []store.Device{
		{ISPID: isp.ID, DeviceID: 8, Name: "nas", ExporterIP: "198.51.100.178"},
		{ISPID: isp.ID, DeviceID: 8, Name: "wan2", ExporterIP: "198.51.100.62"},
		{ISPID: other.ID, DeviceID: 8, Name: "other-tenant", ExporterIP: "203.0.113.90"},
	} {
		if _, err := st.CreateDevice(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	rows := []natRecord{{ISPID: isp.ID, DevID: 8, ExporterIP: "198.51.100.178"}, {ISPID: isp.ID, DevID: 8, ExporterIP: "198.51.100.62"}, {ISPID: other.ID, DevID: 8, ExporterIP: "203.0.113.90"}, {ISPID: isp.ID, DevID: 8, Sub: "DEV-8"}}
	s.nameDevices(ctx, 0, rows)
	if rows[0].Sub != "nas" || rows[1].Sub != "wan2" || rows[2].Sub != "other-tenant" || rows[3].Sub != "DEV-8" {
		t.Fatalf("ambiguous exporter identity: %+v", rows)
	}
}

func TestSourceNATFiltersPreserveRawDirectionAndExporterScope(t *testing.T) {
	f := SearchFilter{ISPID: 5, DeviceID: 8, ExporterIP: "198.51.100.178", PublicIP: "203.0.113.176", PublicPort: 42286, destinationNATAvailable: true}
	where, args, ok := hotWhere(f)
	if !ok || strings.Contains(where, "nat_dest") || !strings.Contains(where, "exporter_ip = toIPv4(?)") {
		t.Fatalf("wrong source-NAT/exporter filter: %s", where)
	}
	want := []any{f.PublicIP, uint16(42286), uint32(8), f.ExporterIP, uint32(5)}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("wrong filter arguments: %#v", args)
	}
	conds, cargs, ok := coldWhere(f, "flow_start")
	if !ok || !strings.Contains(strings.Join(conds, " "), "exporter_ip = ?") || !reflect.DeepEqual(cargs, want[:len(want)-1]) {
		t.Fatalf("cold filter differs: %v %#v", conds, cargs)
	}
	// A device-only search must not filter out stored rows with absent NAT fields.
	f.PublicIP, f.PublicPort = "", 0
	where, _, _ = hotWhere(f)
	if strings.Contains(where, "nat_public") || strings.Contains(where, "nat_dest") {
		t.Fatalf("implicit NAT gate hides stored records: %s", where)
	}
}

func TestCRMDestinationUsesInternalTupleAndActualExporter(t *testing.T) {
	var received crmBatchRequest
	crm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
			return
		}
		resp := crmBatchResponse{SchemaVersion: crmSchemaVersion, RequestID: received.RequestID}
		for _, q := range received.Lookups {
			resp.Results = append(resp.Results, crmResult{ReferenceCode: q.ReferenceCode, Status: "not_found"})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer crm.Close()
	s, st := testServer(t)
	isp, dev := seedCRMISP(t, st)
	_, err := st.CreateDevice(context.Background(), store.Device{ISPID: isp.ID, DeviceID: dev.DeviceID, Name: "other-uplink", ExporterIP: "198.51.100.11"})
	if err != nil {
		t.Fatal(err)
	}
	s.InitSettings(context.Background(), Settings{CRMConnectors: []CRMConnectorSettings{{ISPID: isp.ID, Enabled: true, Endpoint: crm.URL, APIKey: "x", TimeoutMs: 2000, BatchSize: 100}}})
	r := observedDestinationRecord()
	r.ISPID, r.DevID, r.ExporterIP, r.At = isp.ID, dev.DeviceID, dev.ExporterIP, time.Now()
	r.setNATTranslation()
	old := r
	old.PostDstIP = ""
	old.PostDstPort = 0
	old.setNATTranslation()
	rows := []natRecord{r, old}
	sum := s.enrichCRMRows(context.Background(), isp.ID, rows)
	if len(received.Lookups) != 1 {
		t.Fatalf("wrong lookup count: %+v", received)
	}
	q := received.Lookups[0]
	if q.LocalIP != "10.0.0.12" || q.NASIPAddress != dev.ExporterIP || q.NASIdentifier != dev.Name {
		t.Fatalf("wrong identity lookup: %+v", q)
	}
	if rows[1].CRMStatus != "insufficient_nat_data" || sum.Failed != 1 {
		t.Fatalf("unknown NAT evidence caused lookup: %+v %+v", rows, sum)
	}
}
