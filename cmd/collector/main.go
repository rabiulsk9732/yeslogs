// Command collector is the NATFlow dataplane: it receives NetFlow/IPFIX over
// UDP, decodes and normalizes flows, applies skip rules, and batch-inserts the
// survivors into ClickHouse via a sharded writer pool, exposing Prometheus
// metrics throughout. It supports SIGHUP/file-watch config reload, an S3 health
// check, and one-day S3 archive export.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/natflow/natflow-dataplane/internal/archive"
	"github.com/natflow/natflow-dataplane/internal/archive/s3"
	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/decoder/ipfix"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow9"
	devreg "github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/logger"
	"github.com/natflow/natflow-dataplane/internal/managed"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	chwriter "github.com/natflow/natflow-dataplane/internal/writer/clickhouse"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	cfgPath := flag.String("config", "configs/collector.yaml", "path to the YAML config file")
	s3Check := flag.Bool("s3-check", false, "upload an S3 health marker and exit")
	archiveDay := flag.String("archive-day", "", "export this day (YYYY-MM-DD) to S3 and exit")
	watchConfig := flag.Bool("watch-config", false, "reload config when the file changes")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	log, levelVar, closeLog, err := logger.New(cfg.Logging.Level, cfg.Logging.File)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog.Close() }()
	log = log.With("server", cfg.Server.Name)

	m := metrics.New()

	// One-shot modes.
	if *s3Check {
		return runS3Check(cfg, log)
	}
	if *archiveDay != "" {
		return runArchive(cfg, *archiveDay, m, log)
	}

	log.Info("starting natflow-dataplane collector",
		"isp_id", cfg.Server.ISPID, "clickhouse", cfg.ClickHouse.Addr,
		"writer_workers", cfg.ClickHouse.WriterWorkers, "backpressure", cfg.Pipeline.BackpressureMode)

	metricsSrv := m.NewServer(cfg.Metrics.Bind)
	srvErr := make(chan error, 1)
	metricsSrv.Start(srvErr)
	log.Info("metrics endpoint listening", "addr", cfg.Metrics.Bind)
	defer func() {
		if err != nil {
			sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = metricsSrv.Shutdown(sc)
			cancel()
		}
	}()

	live := config.NewStore(cfg.Live())
	manager, err := chwriter.NewManager(cfg.ClickHouse, live, m, log)
	if err != nil {
		return fmt.Errorf("init writer: %w", err)
	}
	log.Info("writer pool started",
		"workers", cfg.ClickHouse.WriterWorkers, "batch_size", cfg.ClickHouse.BatchSize,
		"flush_interval", cfg.ClickHouse.FlushInterval(), "max_queue_rows", cfg.ClickHouse.MaxQueueRows)

	norm := normalizer.New()
	registry, derr := devreg.Build(cfg.Devices, cfg.Live().Rules)
	if derr != nil {
		return fmt.Errorf("device registry: %w", derr)
	}
	devices := devreg.NewStore(registry)
	m.DeviceRegistryEntries.Set(float64(registry.Len()))
	log.Info("device registry loaded",
		"entries", registry.Len(), "unknown_exporter_mode", cfg.Security.UnknownExporterMode)

	// Managed mode: pull the device registry + policy from the Director.
	var mc *managed.Client
	managedCtx, managedCancel := context.WithCancel(context.Background())
	defer managedCancel()
	if cfg.Director.Managed() {
		mc = managed.New(cfg.Director, devices, live, cfg.Live().Rules, m, log)
		pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
		if perr := mc.PollOnce(pctx); perr != nil {
			log.Warn("initial director pull failed; starting with local/empty registry", "error", perr)
		}
		pcancel()
		go mc.Run(managedCtx, time.Duration(cfg.Director.PollIntervalS)*time.Second)
		log.Info("managed mode enabled", "director", cfg.Director.URL, "poll_s", cfg.Director.PollIntervalS)
	}

	bindings := []struct {
		name string
		port int
		dec  decoder.Decoder
	}{
		{"netflow5", cfg.Receiver.Ports.NetFlow5, netflow5.New()},
		{"netflow9", cfg.Receiver.Ports.NetFlow9, netflow9.New(m.TemplatesReceived, m.TemplateUnknown)},
		{"ipfix", cfg.Receiver.Ports.IPFIX, ipfix.New(m.TemplatesReceived, m.TemplateUnknown)},
	}

	var receivers []*receiver.Receiver
	for _, b := range bindings {
		p := pipeline.New(b.dec, norm, live, devices, cfg.Server.ISPID, cfg.Server.DeviceIDDefault, manager, m, log)
		rcv, rerr := receiver.New(b.name, cfg.Receiver.BindIP, b.port,
			cfg.Receiver.Workers, cfg.Receiver.UDPReadBufferMB, p, m, log)
		if rerr != nil {
			for _, r := range receivers {
				r.Stop()
			}
			manager.Stop()
			return rerr
		}
		receivers = append(receivers, rcv)
	}
	for _, r := range receivers {
		r.Start()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	reloadCh := make(chan struct{}, 1)
	trigger := func() {
		select {
		case reloadCh <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				trigger()
			}
		}
	}()
	if *watchConfig {
		go watchConfigFile(ctx, *cfgPath, trigger, log)
	}

	current := cfg
	reload := func() {
		next, lerr := config.Load(*cfgPath)
		if lerr != nil {
			m.ConfigReloadErrors.Inc()
			log.Error("config reload failed; keeping running config", "error", lerr)
			return
		}
		if changed := config.NonReloadableChanges(current, next); len(changed) > 0 {
			log.Warn("ignoring non-reloadable config changes (restart required)", "fields", changed)
		}
		live.Store(next.Live())
		levelVar.Set(logger.ParseLevel(next.Logging.Level))
		manager.Reload(next.ClickHouse.WriterWorkers, next.ClickHouse.MaxQueueRows)

		// Refresh the device registry. In managed mode it comes from the
		// Director (re-pull); otherwise rebuild from local config.
		if mc != nil {
			if perr := mc.PollOnce(context.Background()); perr != nil {
				m.DeviceRegistryReloadErrors.Inc()
				log.Error("director pull on reload failed; keeping previous registry", "error", perr)
			} else {
				m.DeviceRegistryReloads.Inc()
			}
		} else if reg, rerr := devreg.Build(next.Devices, next.Live().Rules); rerr != nil {
			m.DeviceRegistryReloadErrors.Inc()
			log.Error("device registry reload failed; keeping previous registry", "error", rerr)
		} else {
			devices.Store(reg)
			m.DeviceRegistryEntries.Set(float64(reg.Len()))
			m.DeviceRegistryReloads.Inc()
		}

		current = next
		m.ConfigReloads.Inc()
		log.Info("config reloaded",
			"batch_size", next.ClickHouse.BatchSize, "flush_interval", next.ClickHouse.FlushInterval(),
			"writer_workers", next.ClickHouse.WriterWorkers, "max_queue_rows", next.ClickHouse.MaxQueueRows,
			"backpressure", next.Pipeline.BackpressureMode, "level", next.Logging.Level,
			"devices", devices.Load().Len(), "unknown_exporter_mode", next.Security.UnknownExporterMode)
	}

	log.Info("collector running; SIGHUP=reload, SIGINT/SIGTERM=stop")
loop:
	for {
		select {
		case <-ctx.Done():
			log.Info("shutdown signal received")
			break loop
		case e := <-srvErr:
			log.Error("metrics server failed", "error", e)
			break loop
		case <-reloadCh:
			reload()
		}
	}

	for _, r := range receivers {
		r.Stop()
	}
	manager.Stop()
	log.Info("writers drained")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if serr := metricsSrv.Shutdown(shutCtx); serr != nil {
		log.Warn("metrics server shutdown error", "error", serr)
	}
	log.Info("shutdown complete")
	return nil
}

// watchConfigFile polls the config file's modification time and triggers a
// reload when it changes.
func watchConfigFile(ctx context.Context, path string, trigger func(), log *slog.Logger) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var last time.Time
	if fi, err := os.Stat(path); err == nil {
		last = fi.ModTime()
	}
	log.Info("watching config file for changes", "path", path)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fi, err := os.Stat(path)
			if err != nil {
				continue
			}
			if mt := fi.ModTime(); mt.After(last) {
				last = mt
				trigger()
			}
		}
	}
}

func runS3Check(cfg *config.Config, log *slog.Logger) error {
	client, err := s3.New(s3.Config{
		Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket,
		AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, PathPrefix: cfg.S3.PathPrefix,
	})
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	key, err := client.HealthMarker(ctx, cfg.Server.Name, time.Now())
	if err != nil {
		return fmt.Errorf("s3 health marker upload: %w", err)
	}
	log.Info("s3 health marker uploaded", "bucket", cfg.S3.Bucket, "key", key)
	fmt.Printf("OK: uploaded s3://%s/%s\n", cfg.S3.Bucket, key)
	return nil
}

func runArchive(cfg *config.Config, dateStr string, m *metrics.Metrics, log *slog.Logger) error {
	day, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return fmt.Errorf("invalid --archive-day %q (want YYYY-MM-DD): %w", dateStr, err)
	}
	client, err := s3.New(s3.Config{
		Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket,
		AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, PathPrefix: cfg.S3.PathPrefix,
	})
	if err != nil {
		return fmt.Errorf("s3 client: %w", err)
	}
	conn, err := chwriter.Connect(cfg.ClickHouse)
	if err != nil {
		return err
	}
	defer conn.Close()

	exp := archive.New(conn, client, cfg.ClickHouse.Database, "flow_logs", m, log)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := exp.ExportDay(ctx, cfg.Server.ISPID, day, cfg.S3.ExportFormat)
	if err != nil {
		return fmt.Errorf("archive %s: %w", dateStr, err)
	}
	fmt.Printf("OK: archived %s isp_id=%d rows=%d bytes=%d -> s3://%s/%s\n",
		dateStr, cfg.Server.ISPID, res.Rows, res.Bytes, cfg.S3.Bucket, res.Key)
	return nil
}
