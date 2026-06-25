// Package config loads, defaults and validates the collector's YAML
// configuration.
package config

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"time"

	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration, mirroring configs/collector.yaml.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Receiver   ReceiverConfig   `yaml:"receiver"`
	ClickHouse ClickHouseConfig `yaml:"clickhouse"`
	Rules      RulesConfig      `yaml:"rules"`
	Metrics    MetricsConfig    `yaml:"metrics"`
	Logging    LoggingConfig    `yaml:"logging"`
}

// ServerConfig identifies this collector instance.
type ServerConfig struct {
	Name            string `yaml:"name"`
	ISPID           uint32 `yaml:"isp_id"`
	DeviceIDDefault uint32 `yaml:"device_id_default"`
}

// PortsConfig is the per-protocol UDP listen ports.
type PortsConfig struct {
	NetFlow5 int `yaml:"netflow5"`
	NetFlow9 int `yaml:"netflow9"`
	IPFIX    int `yaml:"ipfix"`
}

// ReceiverConfig configures the UDP listeners.
type ReceiverConfig struct {
	BindIP          string      `yaml:"bind_ip"`
	Ports           PortsConfig `yaml:"ports"`
	UDPReadBufferMB int         `yaml:"udp_read_buffer_mb"`
	Workers         int         `yaml:"workers"`
}

// ClickHouseConfig configures the batching writer.
type ClickHouseConfig struct {
	Addr            string `yaml:"addr"`
	Database        string `yaml:"database"`
	Username        string `yaml:"username"`
	Password        string `yaml:"password"`
	BatchSize       int    `yaml:"batch_size"`
	FlushIntervalMS int    `yaml:"flush_interval_ms"`
	QueueCapacity   int    `yaml:"queue_capacity"`
	ShutdownDrainMS int    `yaml:"shutdown_drain_ms"`
}

// RulesConfig toggles the skip filters.
type RulesConfig struct {
	SkipDNS              bool `yaml:"skip_dns"`
	SkipPrivateToPrivate bool `yaml:"skip_private_to_private"`
	SkipZeroBytes        bool `yaml:"skip_zero_bytes"`
}

// MetricsConfig configures the Prometheus endpoint.
type MetricsConfig struct {
	Bind string `yaml:"bind"`
}

// LoggingConfig configures logging.
type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

// FlushInterval returns the configured flush interval as a duration.
func (c ClickHouseConfig) FlushInterval() time.Duration {
	return time.Duration(c.FlushIntervalMS) * time.Millisecond
}

// ShutdownDrain returns the maximum time to drain the writer queue on shutdown.
func (c ClickHouseConfig) ShutdownDrain() time.Duration {
	return time.Duration(c.ShutdownDrainMS) * time.Millisecond
}

// Load reads, parses, defaults and validates the config at path. Unknown YAML
// keys are rejected so typos surface immediately.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Receiver.BindIP == "" {
		c.Receiver.BindIP = "0.0.0.0"
	}
	if c.Receiver.Ports.NetFlow5 == 0 {
		c.Receiver.Ports.NetFlow5 = 2055
	}
	if c.Receiver.Ports.NetFlow9 == 0 {
		c.Receiver.Ports.NetFlow9 = 9995
	}
	if c.Receiver.Ports.IPFIX == 0 {
		c.Receiver.Ports.IPFIX = 4739
	}
	if c.Receiver.UDPReadBufferMB <= 0 {
		c.Receiver.UDPReadBufferMB = 64
	}
	if c.Receiver.Workers <= 0 {
		c.Receiver.Workers = runtime.NumCPU()
	}
	if c.ClickHouse.Addr == "" {
		c.ClickHouse.Addr = "127.0.0.1:9000"
	}
	if c.ClickHouse.Database == "" {
		c.ClickHouse.Database = "natlogs"
	}
	if c.ClickHouse.Username == "" {
		c.ClickHouse.Username = "default"
	}
	if c.ClickHouse.BatchSize <= 0 {
		c.ClickHouse.BatchSize = 10000
	}
	if c.ClickHouse.FlushIntervalMS <= 0 {
		c.ClickHouse.FlushIntervalMS = 1000
	}
	if c.ClickHouse.QueueCapacity <= 0 {
		// Buffer roughly ten batches in flight before back-pressure/drop.
		c.ClickHouse.QueueCapacity = c.ClickHouse.BatchSize * 10
	}
	if c.ClickHouse.ShutdownDrainMS <= 0 {
		c.ClickHouse.ShutdownDrainMS = 15000
	}
	if c.Metrics.Bind == "" {
		c.Metrics.Bind = "127.0.0.1:9101"
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
}

func (c *Config) validate() error {
	if net.ParseIP(c.Receiver.BindIP) == nil {
		return fmt.Errorf("receiver.bind_ip %q is not a valid IP", c.Receiver.BindIP)
	}
	ports := map[string]int{
		"netflow5": c.Receiver.Ports.NetFlow5,
		"netflow9": c.Receiver.Ports.NetFlow9,
		"ipfix":    c.Receiver.Ports.IPFIX,
	}
	seen := map[int]string{}
	for name, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("receiver.ports.%s (%d) out of range 1-65535", name, p)
		}
		if other, dup := seen[p]; dup {
			return fmt.Errorf("receiver.ports.%s and receiver.ports.%s both use port %d", name, other, p)
		}
		seen[p] = name
	}
	if c.ClickHouse.QueueCapacity < c.ClickHouse.BatchSize {
		return fmt.Errorf("clickhouse.queue_capacity (%d) must be >= batch_size (%d)",
			c.ClickHouse.QueueCapacity, c.ClickHouse.BatchSize)
	}
	return nil
}
