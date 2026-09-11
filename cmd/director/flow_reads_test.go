package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFlowReadGatewayConfigRequiresReaderAndUpstream(t *testing.T) {
	base := "session_key: test-session-key-at-least-16\nmysql_dsn: test\nflow_reads: true\n"
	for _, tc := range []struct {
		extra string
		valid bool
	}{
		{"", false},
		{"upstream: http://127.0.0.1:8080\n", false},
		{"clickhouse:\n  addr: 127.0.0.1:9000\n", false},
		{"upstream: http://127.0.0.1:8080\nclickhouse:\n  addr: 127.0.0.1:9000\n", true},
	} {
		path := filepath.Join(t.TempDir(), "management.yaml")
		if err := os.WriteFile(path, []byte(base+tc.extra), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := loadConfig(path)
		if (err == nil) != tc.valid {
			t.Fatalf("config validation: valid=%v err=%v", tc.valid, err)
		}
	}
}

func TestFlowReadDefaultsReadCollectorS3BootstrapOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.yaml")
	if err := os.WriteFile(path, []byte("receiver:\n  ports:\n    netflow9: 9995\ns3:\n  enabled: true\n  endpoint: https://s3.invalid\n  bucket: logs\n  access_key: test-access\n  secret_key: test-secret\n  path_prefix: archive\n  export_format: parquet\n"), 0600); err != nil {
		t.Fatal(err)
	}
	defaults, err := loadFlowReadDefaults(path)
	if err != nil {
		t.Fatal(err)
	}
	if !defaults.S3.Enabled || defaults.S3.Bucket != "logs" || defaults.S3.AccessKey != "test-access" || defaults.S3.SecretKey != "test-secret" || defaults.S3.PathPrefix != "archive" || defaults.S3.ExportFormat != "parquet" {
		t.Fatal("collector S3 bootstrap defaults were not preserved")
	}
	if _, err := loadFlowReadDefaults(path + ".missing"); err == nil {
		t.Fatal("missing bootstrap config ignored")
	}
}
