package config

import "testing"

func base() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func TestDefaultsAndValidate(t *testing.T) {
	c := base()
	if err := c.validate(); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
	if c.ClickHouse.WriterWorkers != 4 {
		t.Errorf("writer_workers default = %d, want 4", c.ClickHouse.WriterWorkers)
	}
	if c.ClickHouse.MaxQueueRows != 100000 {
		t.Errorf("max_queue_rows default = %d, want 100000", c.ClickHouse.MaxQueueRows)
	}
	if c.Pipeline.BackpressureMode != "drop_new" {
		t.Errorf("backpressure default = %q", c.Pipeline.BackpressureMode)
	}
	if c.S3.ExportFormat != "csvgz" {
		t.Errorf("export_format default = %q", c.S3.ExportFormat)
	}
	if c.ClickHouse.Compression != "lz4" {
		t.Errorf("compression default = %q", c.ClickHouse.Compression)
	}
}

func TestTuningDefaultsAndValidation(t *testing.T) {
	// wait_for_async_insert defaults to true (durable) when unset.
	c := base()
	if !c.Live().WaitForAsyncInsert {
		t.Error("wait_for_async_insert should default to true")
	}
	f := false
	c.ClickHouse.WaitForAsyncInsert = &f
	c.ClickHouse.AsyncInsert = true
	l := c.Live()
	if l.WaitForAsyncInsert || !l.AsyncInsert {
		t.Errorf("async/wait mapping wrong: async=%v wait=%v", l.AsyncInsert, l.WaitForAsyncInsert)
	}

	bad := base()
	bad.ClickHouse.Compression = "snappy"
	if err := bad.validate(); err == nil {
		t.Error("want error for invalid compression")
	}
}

func TestQueueCapacityLegacyAlias(t *testing.T) {
	c := &Config{}
	c.ClickHouse.QueueCapacity = 50000
	c.applyDefaults()
	if c.ClickHouse.MaxQueueRows != 50000 {
		t.Errorf("legacy queue_capacity not mapped: max_queue_rows=%d", c.ClickHouse.MaxQueueRows)
	}
}

func TestValidateErrors(t *testing.T) {
	t.Run("queue<batch", func(t *testing.T) {
		c := base()
		c.ClickHouse.BatchSize = 20000
		c.ClickHouse.MaxQueueRows = 1000
		if err := c.validate(); err == nil {
			t.Error("want error when max_queue_rows < batch_size")
		}
	})
	t.Run("bad backpressure", func(t *testing.T) {
		c := base()
		c.Pipeline.BackpressureMode = "bogus"
		if err := c.validate(); err == nil {
			t.Error("want error for invalid backpressure_mode")
		}
	})
	t.Run("s3 enabled no bucket", func(t *testing.T) {
		c := base()
		c.S3.Enabled = true
		c.S3.Endpoint = "http://x:9000"
		if err := c.validate(); err == nil {
			t.Error("want error when s3.enabled without bucket")
		}
	})
	t.Run("bad export format", func(t *testing.T) {
		c := base()
		c.S3.ExportFormat = "orc"
		if err := c.validate(); err == nil {
			t.Error("want error for invalid export_format")
		}
	})
}

func TestParseBackpressure(t *testing.T) {
	cases := map[string]BackpressureMode{
		"":         DropNew,
		"drop_new": DropNew,
		"drop_old": DropOld,
		"block":    Block,
	}
	for in, want := range cases {
		got, err := ParseBackpressure(in)
		if err != nil || got != want {
			t.Errorf("ParseBackpressure(%q) = %v,%v want %v", in, got, err, want)
		}
	}
	if _, err := ParseBackpressure("nope"); err == nil {
		t.Error("want error for invalid mode")
	}
}

func TestNonReloadableChanges(t *testing.T) {
	old := base()
	next := base()
	if c := NonReloadableChanges(old, next); len(c) != 0 {
		t.Errorf("identical configs should report no changes, got %v", c)
	}
	next.Receiver.BindIP = "10.0.0.1"
	next.ClickHouse.Addr = "other:9000"
	next.Receiver.Ports.NetFlow5 = 3055
	next.ClickHouse.Compression = "zstd"
	next.ClickHouse.MaxOpenConns = 99
	c := NonReloadableChanges(old, next)
	want := map[string]bool{
		"receiver.bind_ip": true, "clickhouse.addr": true, "receiver.ports": true,
		"clickhouse.compression": true, "clickhouse.max_open_conns": true,
	}
	for _, name := range c {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Errorf("missing detected changes: %v (got %v)", want, c)
	}
}

func TestLiveMapping(t *testing.T) {
	c := base()
	c.Rules.SkipDNS = true
	c.ClickHouse.BatchSize = 5000
	c.ClickHouse.RetryMaxAttempts = 7
	c.Pipeline.BackpressureMode = "drop_old"
	c.S3.Enabled = true
	l := c.Live()
	if !l.Rules.SkipDNS {
		t.Error("rules not mapped")
	}
	if l.BatchSize != 5000 || l.RetryMaxAttempts != 7 {
		t.Errorf("ch fields not mapped: batch=%d retry=%d", l.BatchSize, l.RetryMaxAttempts)
	}
	if l.Backpressure != DropOld {
		t.Errorf("backpressure not mapped: %v", l.Backpressure)
	}
	if !l.S3Enabled {
		t.Error("s3 enabled not mapped")
	}
}

func TestStoreAtomicSwap(t *testing.T) {
	s := NewStore(Live{BatchSize: 1})
	if s.Load().BatchSize != 1 {
		t.Fatal("initial load")
	}
	s.Store(Live{BatchSize: 2})
	if s.Load().BatchSize != 2 {
		t.Fatal("store/load")
	}
}
