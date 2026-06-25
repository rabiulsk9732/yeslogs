// Package config loads, defaults and validates the collector's YAML
// configuration, and exposes the hot-reloadable subset via an atomic Store.
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/rules"
)

// Config is the top-level configuration, mirroring configs/collector.yaml.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Receiver   ReceiverConfig   `yaml:"receiver"`
	ClickHouse ClickHouseConfig `yaml:"clickhouse"`
	Rules      RulesConfig      `yaml:"rules"`
	Pipeline   PipelineConfig   `yaml:"pipeline"`
	S3         S3Config         `yaml:"s3"`
	Security   SecurityConfig   `yaml:"security"`
	Devices    []device.Spec    `yaml:"devices"`
	Director   DirectorConfig   `yaml:"director"`
	CP         CPConfig         `yaml:"cp"`
	Metrics    MetricsConfig    `yaml:"metrics"`
	Logging    LoggingConfig    `yaml:"logging"`
}

// CPConfig is the control-plane section, used only by the unified natlog service
// (the standalone collector ignores it).
type CPConfig struct {
	Bind             string `yaml:"bind"`
	SessionKey       string `yaml:"session_key"`
	CookieSecure     bool   `yaml:"cookie_secure"`
	MySQLDSN         string `yaml:"mysql_dsn"`
	FlowDays         int    `yaml:"flow_days"`
	RegistryRefreshS int    `yaml:"registry_refresh_s"`
	RetentionDays    int    `yaml:"retention_days"`
}

// DirectorConfig enables managed mode: the collector pulls its device registry
// and exporter policy from the Director control plane instead of local config.
type DirectorConfig struct {
	URL           string `yaml:"url"`             // e.g. http://director:8080
	Token         string `yaml:"token"`           // agent bearer token
	PollIntervalS int    `yaml:"poll_interval_s"` // default 30
}

// Managed reports whether the collector should pull config from the Director.
func (c DirectorConfig) Managed() bool { return c.URL != "" && c.Token != "" }

// SecurityConfig holds exporter-trust settings.
type SecurityConfig struct {
	UnknownExporterMode string `yaml:"unknown_exporter_mode"` // allow | observe | reject
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

// ClickHouseConfig configures the sharded batching writer pool.
type ClickHouseConfig struct {
	Addr             string `yaml:"addr"`
	Database         string `yaml:"database"`
	Username         string `yaml:"username"`
	Password         string `yaml:"password"`
	BatchSize        int    `yaml:"batch_size"`
	FlushIntervalMS  int    `yaml:"flush_interval_ms"`
	WriterWorkers    int    `yaml:"writer_workers"`
	MaxQueueRows     int    `yaml:"max_queue_rows"` // per-writer queue capacity
	RetryMaxAttempts int    `yaml:"retry_max_attempts"`
	RetryBackoffMS   int    `yaml:"retry_backoff_ms"`
	ShutdownDrainMS  int    `yaml:"shutdown_drain_ms"`
	QueueCapacity    int    `yaml:"queue_capacity"` // deprecated alias for max_queue_rows

	// Tuning (v0.4.0).
	Compression              string `yaml:"compression"`           // lz4 | lz4hc | zstd | none (native protocol)
	MaxOpenConns             int    `yaml:"max_open_conns"`        // 0 = auto (writer_workers*2+2, min 8)
	AsyncInsert              bool   `yaml:"async_insert"`          // use ClickHouse server-side async inserts
	WaitForAsyncInsert       *bool  `yaml:"wait_for_async_insert"` // nil => true (durable); false => fire-and-forget
	AsyncInsertBusyTimeoutMS int    `yaml:"async_insert_busy_timeout_ms"`
}

// RulesConfig toggles the skip filters.
type RulesConfig struct {
	SkipDNS              bool `yaml:"skip_dns"`
	SkipPrivateToPrivate bool `yaml:"skip_private_to_private"`
	SkipZeroBytes        bool `yaml:"skip_zero_bytes"`
}

// PipelineConfig configures pipeline behavior.
type PipelineConfig struct {
	BackpressureMode string `yaml:"backpressure_mode"` // block | drop_new | drop_old
}

// S3Config configures the S3-compatible archive target.
type S3Config struct {
	Enabled          bool   `yaml:"enabled"`
	Endpoint         string `yaml:"endpoint"`
	Region           string `yaml:"region"`
	Bucket           string `yaml:"bucket"`
	AccessKey        string `yaml:"access_key"`
	SecretKey        string `yaml:"secret_key"`
	PathPrefix       string `yaml:"path_prefix"`
	ArchiveAfterDays int    `yaml:"archive_after_days"`
	ExportFormat     string `yaml:"export_format"` // csvgz | parquet
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

// RetryBackoff returns the base retry backoff as a duration.
func (c ClickHouseConfig) RetryBackoff() time.Duration {
	return time.Duration(c.RetryBackoffMS) * time.Millisecond
}

// ShutdownDrain returns the maximum time to drain the writer queues on shutdown.
func (c ClickHouseConfig) ShutdownDrain() time.Duration {
	return time.Duration(c.ShutdownDrainMS) * time.Millisecond
}

// BackpressureMode selects how a full writer queue sheds load.
type BackpressureMode int

const (
	// DropNew drops the incoming record when the queue is full (default).
	DropNew BackpressureMode = iota
	// DropOld evicts the oldest queued record to make room for the new one.
	DropOld
	// Block applies back-pressure by blocking the producer until there is room.
	Block
)

func (m BackpressureMode) String() string {
	switch m {
	case DropOld:
		return "drop_old"
	case Block:
		return "block"
	default:
		return "drop_new"
	}
}

// ParseBackpressure parses a backpressure mode string.
func ParseBackpressure(s string) (BackpressureMode, error) {
	switch s {
	case "", "drop_new":
		return DropNew, nil
	case "drop_old":
		return DropOld, nil
	case "block":
		return Block, nil
	default:
		return DropNew, fmt.Errorf("invalid backpressure_mode %q (block|drop_new|drop_old)", s)
	}
}

// Live is the hot-reloadable subset of configuration, read atomically by the
// running pipeline and writers.
type Live struct {
	Rules            rules.RuleSet
	BatchSize        int
	FlushInterval    time.Duration
	RetryMaxAttempts int
	RetryBackoff     time.Duration
	Backpressure     BackpressureMode
	UnknownMode      device.UnknownMode

	AsyncInsert              bool
	WaitForAsyncInsert       bool
	AsyncInsertBusyTimeoutMS int

	S3Enabled          bool
	S3ArchiveAfterDays int
}

// Live extracts the reloadable subset from the full config.
func (c *Config) Live() Live {
	mode, _ := ParseBackpressure(c.Pipeline.BackpressureMode)
	unknown, _ := device.ParseUnknownMode(c.Security.UnknownExporterMode)
	return Live{
		UnknownMode: unknown,
		Rules: rules.RuleSet{
			SkipDNS:              c.Rules.SkipDNS,
			SkipPrivateToPrivate: c.Rules.SkipPrivateToPrivate,
			SkipZeroBytes:        c.Rules.SkipZeroBytes,
		},
		BatchSize:                c.ClickHouse.BatchSize,
		FlushInterval:            c.ClickHouse.FlushInterval(),
		RetryMaxAttempts:         c.ClickHouse.RetryMaxAttempts,
		RetryBackoff:             c.ClickHouse.RetryBackoff(),
		Backpressure:             mode,
		AsyncInsert:              c.ClickHouse.AsyncInsert,
		WaitForAsyncInsert:       c.ClickHouse.WaitForAsyncInsert == nil || *c.ClickHouse.WaitForAsyncInsert,
		AsyncInsertBusyTimeoutMS: c.ClickHouse.AsyncInsertBusyTimeoutMS,
		S3Enabled:                c.S3.Enabled,
		S3ArchiveAfterDays:       c.S3.ArchiveAfterDays,
	}
}

// Store holds the current Live config behind an atomic pointer.
type Store struct {
	v atomic.Pointer[Live]
}

// NewStore creates a Store seeded with l.
func NewStore(l Live) *Store {
	s := &Store{}
	s.v.Store(&l)
	return s
}

// Load returns the current Live config (read-only; do not mutate).
func (s *Store) Load() *Live { return s.v.Load() }

// Store atomically replaces the current Live config.
func (s *Store) Store(l Live) { s.v.Store(&l) }

// NonReloadableChanges returns the names of immutable fields that differ between
// old and next. A non-empty result means a reload cannot fully apply.
func NonReloadableChanges(old, next *Config) []string {
	var changed []string
	add := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	add("receiver.bind_ip", old.Receiver.BindIP != next.Receiver.BindIP)
	add("receiver.ports", old.Receiver.Ports != next.Receiver.Ports)
	add("receiver.workers", old.Receiver.Workers != next.Receiver.Workers)
	add("clickhouse.addr", old.ClickHouse.Addr != next.ClickHouse.Addr)
	add("clickhouse.database", old.ClickHouse.Database != next.ClickHouse.Database)
	add("clickhouse.username", old.ClickHouse.Username != next.ClickHouse.Username)
	add("clickhouse.password", old.ClickHouse.Password != next.ClickHouse.Password)
	add("clickhouse.compression", old.ClickHouse.Compression != next.ClickHouse.Compression)
	add("clickhouse.max_open_conns", old.ClickHouse.MaxOpenConns != next.ClickHouse.MaxOpenConns)
	add("metrics.bind", old.Metrics.Bind != next.Metrics.Bind)
	return changed
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
	if c.ClickHouse.WriterWorkers <= 0 {
		c.ClickHouse.WriterWorkers = 4
	}
	if c.ClickHouse.MaxQueueRows <= 0 {
		if c.ClickHouse.QueueCapacity > 0 {
			c.ClickHouse.MaxQueueRows = c.ClickHouse.QueueCapacity // legacy alias
		} else {
			c.ClickHouse.MaxQueueRows = 100000
		}
	}
	if c.ClickHouse.RetryMaxAttempts <= 0 {
		c.ClickHouse.RetryMaxAttempts = 3
	}
	if c.ClickHouse.RetryBackoffMS <= 0 {
		c.ClickHouse.RetryBackoffMS = 250
	}
	if c.ClickHouse.ShutdownDrainMS <= 0 {
		c.ClickHouse.ShutdownDrainMS = 15000
	}
	if c.ClickHouse.Compression == "" {
		c.ClickHouse.Compression = "lz4"
	}
	if c.Pipeline.BackpressureMode == "" {
		c.Pipeline.BackpressureMode = "drop_new"
	}
	if c.S3.ExportFormat == "" {
		c.S3.ExportFormat = "csvgz"
	}
	if c.S3.Region == "" {
		c.S3.Region = "us-east-1"
	}
	if c.Director.PollIntervalS <= 0 {
		c.Director.PollIntervalS = 30
	}
	if c.Security.UnknownExporterMode == "" {
		// Default to allow so existing configs without a device registry keep
		// their pre-registry behavior; production should set "reject".
		c.Security.UnknownExporterMode = "allow"
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
	if c.ClickHouse.WriterWorkers < 1 || c.ClickHouse.WriterWorkers > 256 {
		return fmt.Errorf("clickhouse.writer_workers (%d) out of range 1-256", c.ClickHouse.WriterWorkers)
	}
	if c.ClickHouse.MaxQueueRows < c.ClickHouse.BatchSize {
		return fmt.Errorf("clickhouse.max_queue_rows (%d) must be >= batch_size (%d)",
			c.ClickHouse.MaxQueueRows, c.ClickHouse.BatchSize)
	}
	switch c.ClickHouse.Compression {
	case "lz4", "lz4hc", "zstd", "none":
	default:
		// gzip is HTTP-only; the native protocol supports lz4/lz4hc/zstd/none.
		return fmt.Errorf("clickhouse.compression %q must be lz4|lz4hc|zstd|none", c.ClickHouse.Compression)
	}
	if _, err := ParseBackpressure(c.Pipeline.BackpressureMode); err != nil {
		return err
	}
	switch c.S3.ExportFormat {
	case "csvgz", "parquet":
	default:
		return fmt.Errorf("s3.export_format %q must be csvgz or parquet", c.S3.ExportFormat)
	}
	if _, err := device.ParseUnknownMode(c.Security.UnknownExporterMode); err != nil {
		return err
	}
	if err := device.Validate(c.Devices); err != nil {
		return err
	}
	if c.S3.Enabled {
		if c.S3.Bucket == "" {
			return fmt.Errorf("s3.enabled but s3.bucket is empty")
		}
		if c.S3.Endpoint == "" {
			return fmt.Errorf("s3.enabled but s3.endpoint is empty")
		}
	}
	return nil
}
