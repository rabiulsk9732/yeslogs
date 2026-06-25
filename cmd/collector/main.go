// Command collector is the NATFlow dataplane: it receives NetFlow/IPFIX over
// UDP, decodes and normalizes flows, applies skip rules, and batch-inserts the
// survivors into ClickHouse, exposing Prometheus metrics throughout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/decoder/ipfix"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow9"
	"github.com/natflow/natflow-dataplane/internal/logger"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	"github.com/natflow/natflow-dataplane/internal/rules"
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
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	log, closeLog, err := logger.New(cfg.Logging.Level, cfg.Logging.File)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog.Close() }()
	log = log.With("server", cfg.Server.Name)
	log.Info("starting natflow-dataplane collector",
		"isp_id", cfg.Server.ISPID, "clickhouse", cfg.ClickHouse.Addr)

	m := metrics.New()
	metricsSrv := m.NewServer(cfg.Metrics.Bind)
	srvErr := make(chan error, 1)
	metricsSrv.Start(srvErr)
	log.Info("metrics endpoint listening", "addr", cfg.Metrics.Bind)
	// Ensure the metrics listener is torn down on any early-return error path.
	defer func() {
		if err != nil {
			sc, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = metricsSrv.Shutdown(sc)
			cancel()
		}
	}()

	writer, err := chwriter.New(chwriter.Config{
		Addr:            cfg.ClickHouse.Addr,
		Database:        cfg.ClickHouse.Database,
		Username:        cfg.ClickHouse.Username,
		Password:        cfg.ClickHouse.Password,
		BatchSize:       cfg.ClickHouse.BatchSize,
		FlushInterval:   cfg.ClickHouse.FlushInterval(),
		QueueCapacity:   cfg.ClickHouse.QueueCapacity,
		ShutdownTimeout: cfg.ClickHouse.ShutdownDrain(),
	}, m, log)
	if err != nil {
		return fmt.Errorf("init writer: %w", err)
	}
	writer.Start()
	log.Info("clickhouse writer started",
		"batch_size", cfg.ClickHouse.BatchSize,
		"flush_interval", cfg.ClickHouse.FlushInterval())

	norm := normalizer.New(cfg.Server.ISPID, cfg.Server.DeviceIDDefault)
	ruleSet := rules.New(cfg.Rules.SkipDNS, cfg.Rules.SkipPrivateToPrivate, cfg.Rules.SkipZeroBytes)

	// One decoder+pipeline+receiver per protocol; all share writer/norm/rules.
	bindings := []struct {
		name string
		port int
		dec  decoder.Decoder
	}{
		{"netflow5", cfg.Receiver.Ports.NetFlow5, netflow5.New()},
		{"netflow9", cfg.Receiver.Ports.NetFlow9, netflow9.New(m.TemplatesReceived, m.TemplateUnknown)},
		{"ipfix", cfg.Receiver.Ports.IPFIX, ipfix.New()},
	}

	var receivers []*receiver.Receiver
	for _, b := range bindings {
		p := pipeline.New(b.dec, norm, ruleSet, writer, m, log)
		rcv, err := receiver.New(b.name, cfg.Receiver.BindIP, b.port,
			cfg.Receiver.Workers, cfg.Receiver.UDPReadBufferMB, p, m, log)
		if err != nil {
			// Clean up already-bound listeners and the writer before bailing.
			for _, r := range receivers {
				r.Stop()
			}
			writer.Stop()
			return err
		}
		receivers = append(receivers, rcv)
	}
	for _, r := range receivers {
		r.Start()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("collector running; send SIGINT/SIGTERM to stop")
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case err := <-srvErr:
		log.Error("metrics server failed", "error", err)
	}

	// Graceful shutdown: stop ingest first, then drain the writer, then metrics.
	for _, r := range receivers {
		r.Stop()
	}
	writer.Stop()
	log.Info("writer drained")

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := metricsSrv.Shutdown(shutCtx); err != nil {
		log.Warn("metrics server shutdown error", "error", err)
	}
	log.Info("shutdown complete")
	return nil
}
