package main

import (
	"fmt"
	"os"

	"github.com/natflow/natflow-dataplane/internal/director"
	"gopkg.in/yaml.v3"
)

// Read only the collector's S3 bootstrap defaults. DB settings remain the source
// of truth; the gateway never starts receivers or applies runtime configuration.
func loadFlowReadDefaults(path string) (director.Settings, error) {
	var defaults director.Settings
	if path == "" {
		return defaults, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return defaults, fmt.Errorf("read flow_settings_file: %w", err)
	}
	var cfg struct {
		S3 struct {
			Enabled      bool   `yaml:"enabled"`
			Endpoint     string `yaml:"endpoint"`
			Region       string `yaml:"region"`
			Bucket       string `yaml:"bucket"`
			AccessKey    string `yaml:"access_key"`
			SecretKey    string `yaml:"secret_key"`
			PathPrefix   string `yaml:"path_prefix"`
			ExportFormat string `yaml:"export_format"`
		} `yaml:"s3"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return defaults, fmt.Errorf("parse flow_settings_file: %w", err)
	}
	defaults.S3 = director.S3Settings{Enabled: cfg.S3.Enabled, Endpoint: cfg.S3.Endpoint,
		Region: cfg.S3.Region, Bucket: cfg.S3.Bucket, AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey, PathPrefix: cfg.S3.PathPrefix, ExportFormat: cfg.S3.ExportFormat}
	return defaults, nil
}
