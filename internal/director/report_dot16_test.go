package director

import (
	"bytes"
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestReportExport_DoT16Columns_CSV(t *testing.T) {
	meta := reportMeta{
		CaseRef:     "CR-2026-001",
		GeneratedBy: "admin",
		Format:      "dot16",
	}

	at := time.Date(2026, 9, 12, 14, 30, 0, 0, istLoc)
	endAt := time.Date(2026, 9, 12, 14, 35, 0, 0, istLoc)

	rows := []natRecord{
		{
			At:           at,
			EndAt:        endAt,
			PrivIP:       "172.16.10.25",
			PrivPort:     45000,
			PubIP:        "103.204.1.14",
			PubPort:      25000,
			NatIP:        "103.204.1.14",
			NatPort:      25000,
			DstIP:        "142.250.190.46",
			DstPort:      443,
			Proto:        "TCP",
			ExporterIP:   "10.0.0.1",
			CRMUsername:  "rajesh_s",
			CRMMAC:       "00:1A:2B:3C:4D:5E",
			CRMName:      "Rajesh Sharma",
			CRMPhone:     "9876543210",
			CRMAddress:   "Pune, Maharashtra",
			CRMAccountID: "CUST-9901",
		},
	}

	var buf bytes.Buffer
	if err := writeCSV(&buf, meta, rows); err != nil {
		t.Fatalf("writeCSV failed: %v", err)
	}

	r := csv.NewReader(&buf)
	records, err := r.ReadAll()
	if err != nil {
		t.Fatalf("failed to parse CSV: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 rows (header + 1 data), got %d", len(records))
	}

	headers := records[0]
	expectedHeaders := []string{
		"Start Date(mm:dd:yyyy) & Time(hh:mm:ss)",
		"End Date(mm:dd:yyyy) & Time(hh:mm:ss)",
		"Source IP Address",
		"Source Port",
		"Translated IP address",
		"Translated Port",
		"Destination IP Address",
		"Destination Port",
		"Protocol",
		"NAS IP",
		"Username",
		"CallingStationId (MAC)",
		"Customer Name",
		"Phone",
		"Address",
		"Account ID",
	}

	if len(headers) != 16 {
		t.Fatalf("expected 16 headers for dot16 format, got %d: %v", len(headers), headers)
	}

	for i, h := range expectedHeaders {
		if headers[i] != h {
			t.Errorf("header[%d]: expected %q, got %q", i, h, headers[i])
		}
	}

	data := records[1]
	if len(data) != 16 {
		t.Fatalf("expected 16 data columns, got %d", len(data))
	}

	if data[2] != "172.16.10.25" {
		t.Errorf("expected Source IP 172.16.10.25, got %s", data[2])
	}
	if data[4] != "103.204.1.14" {
		t.Errorf("expected Translated IP 103.204.1.14, got %s", data[4])
	}
	if data[8] != "TCP" {
		t.Errorf("expected Protocol TCP, got %s", data[8])
	}
	if data[9] != "10.0.0.1" {
		t.Errorf("expected NAS IP 10.0.0.1, got %s", data[9])
	}
	if data[10] != "rajesh_s" {
		t.Errorf("expected Username rajesh_s, got %s", data[10])
	}
	if data[11] != "00:1A:2B:3C:4D:5E" {
		t.Errorf("expected MAC 00:1A:2B:3C:4D:5E, got %s", data[11])
	}
	if data[12] != "Rajesh Sharma" {
		t.Errorf("expected Name Rajesh Sharma, got %s", data[12])
	}
	if data[13] != "9876543210" {
		t.Errorf("expected Phone 9876543210, got %s", data[13])
	}
}

func TestReportExport_DoT16Columns_FormulaNeutralization(t *testing.T) {
	meta := reportMeta{
		CaseRef: "CR-FORMULA",
		Format:  "dot16",
	}

	rows := []natRecord{
		{
			PrivIP:     "10.0.0.2",
			NatIP:      "103.1.1.1",
			CRMName:    "=cmd|' /C calc'!A0",
			CRMAddress: "+malicious_func()",
			CRMPhone:   "-9999999999",
		},
	}

	var buf bytes.Buffer
	if err := writeCSV(&buf, meta, rows); err != nil {
		t.Fatalf("writeCSV failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, ",=cmd") || strings.Contains(out, "\n=cmd") {
		t.Errorf("formula injection unescaped in name: %s", out)
	}
	if !strings.Contains(out, "'=cmd") {
		t.Errorf("expected escaped '=cmd, got: %s", out)
	}
}

func TestReportExport_DoT16Columns_XLSX_And_PDF(t *testing.T) {
	meta := reportMeta{
		CaseRef: "CR-MEDIA",
		Format:  "dot16",
	}

	rows := []natRecord{
		{
			PrivIP:       "10.0.0.2",
			NatIP:        "103.1.1.1",
			CRMName:      "Test User",
			CRMMAC:       "00:11:22:33:44:55",
			CRMPhone:     "9876543210",
			CRMAddress:   "Test Address",
			CRMSessionID: "SESS-1",
		},
	}

	var xlsxBuf bytes.Buffer
	if err := writeXLSX(&xlsxBuf, meta, rows); err != nil {
		t.Fatalf("writeXLSX failed: %v", err)
	}
	if xlsxBuf.Len() < 500 {
		t.Fatalf("writeXLSX generated unexpectedly small output: %d bytes", xlsxBuf.Len())
	}

	var pdfBuf bytes.Buffer
	if err := writePDF(&pdfBuf, meta, rows); err != nil {
		t.Fatalf("writePDF failed: %v", err)
	}
	if pdfBuf.Len() < 500 {
		t.Fatalf("writePDF generated unexpectedly small output: %d bytes", pdfBuf.Len())
	}
}
