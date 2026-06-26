// Command natlog is the unified NATFlow service: it runs the dataplane
// (NetFlow/IPFIX UDP collector → ClickHouse) and the control plane (the
// multi-tenant Director console + API) in a SINGLE process. The collector's
// device registry is sourced in-process from the Director store, so devices
// added in the UI apply to the dataplane immediately — no HTTP, no agent token.
//
// Bootstrap once:
//
//	natlog --config configs/natlog.yaml --migrate
//	natlog --config configs/natlog.yaml --create-admin --email you@x.com --password '...'
//	natlog --config configs/natlog.yaml          # run DP + CP
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // embed the tz database so Asia/Kolkata resolves in the static binary

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/natflow/natflow-dataplane/internal/archive"
	"github.com/natflow/natflow-dataplane/internal/archive/s3"
	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/decoder/ipfix"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow5"
	"github.com/natflow/natflow-dataplane/internal/decoder/netflow9"
	devreg "github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/director"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"github.com/natflow/natflow-dataplane/internal/logger"
	"github.com/natflow/natflow-dataplane/internal/managed"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/pipeline"
	"github.com/natflow/natflow-dataplane/internal/receiver"
	"github.com/natflow/natflow-dataplane/internal/rules"
	chwriter "github.com/natflow/natflow-dataplane/internal/writer/clickhouse"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "natlog:", err)
		os.Exit(1)
	}
}

func run() (err error) {
	cfgPath := flag.String("config", "configs/natlog.yaml", "path to the unified YAML config")
	migrate := flag.Bool("migrate", false, "run control-plane schema migration and exit")
	createAdmin := flag.Bool("create-admin", false, "create a director admin and exit")
	email := flag.String("email", "", "admin email (with --create-admin)")
	password := flag.String("password", "", "admin password (with --create-admin)")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	cp, err := resolveCP(cfg.CP)
	if err != nil {
		return err
	}

	// Control-plane store (MariaDB/MySQL).
	st, err := store.OpenMySQL(cp.MySQLDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	ctxBg := context.Background()

	switch {
	case *migrate:
		if e := st.Migrate(ctxBg); e != nil {
			return e
		}
		fmt.Println("migration complete")
		return nil
	case *createAdmin:
		if *email == "" || *password == "" {
			return errors.New("--create-admin needs --email and --password")
		}
		if e := st.Migrate(ctxBg); e != nil {
			return e
		}
		hash, e := director.HashPassword(*password)
		if e != nil {
			return e
		}
		if _, e := st.CreateUser(ctxBg, store.User{Email: strings.ToLower(*email), PasswordHash: hash, Role: store.RoleDirector}); e != nil {
			return fmt.Errorf("create admin: %w", e)
		}
		fmt.Printf("created director admin %s\n", *email)
		return nil
	}
	if e := st.Migrate(ctxBg); e != nil {
		return e
	}

	log, _, closeLog, err := logger.New(cfg.Logging.Level, cfg.Logging.File)
	if err != nil {
		return err
	}
	defer func() { _ = closeLog.Close() }()
	log = log.With("server", cfg.Server.Name)
	m := metrics.New()

	log.Info("starting natlog unified service (dataplane + control plane)",
		"clickhouse", cfg.ClickHouse.Addr, "cp_bind", cp.Bind)

	// metrics endpoint
	metricsSrv := m.NewServer(cfg.Metrics.Bind)
	srvErr := make(chan error, 1)
	metricsSrv.Start(srvErr)

	// ---- dataplane ----
	live := config.NewStore(cfg.Live())
	manager, err := chwriter.NewManager(cfg.ClickHouse, live, m, log)
	if err != nil {
		return fmt.Errorf("init writer: %w", err)
	}
	norm := normalizer.New()
	registry, derr := devreg.Build(cfg.Devices, cfg.Live().Rules)
	if derr != nil {
		manager.Stop()
		return fmt.Errorf("device registry: %w", derr)
	}
	devices := devreg.NewStore(registry)

	// ---- control plane (Director) ----
	var fr *director.FlowReader
	fr, ferr := director.NewFlowReader(cfg.ClickHouse.Addr, cfg.ClickHouse.Database, cfg.ClickHouse.Username, cfg.ClickHouse.Password)
	if ferr != nil {
		log.Warn("flow dashboards disabled: clickhouse read unavailable", "error", ferr)
		fr = nil
	}
	dirSrv, err := director.New(director.Config{
		SessionKey: []byte(cp.SessionKey), CookieSecure: cp.CookieSecure, FlowDays: cp.FlowDays,
	}, st, fr, log)
	if err != nil {
		manager.Stop()
		return err
	}
	retDays := cp.RetentionDays
	if retDays <= 0 {
		retDays = 180
	}
	csvOr := func(s string) string {
		if s == "" {
			return "csvgz"
		}
		return s
	}

	// Settings: DB-backed + UI-editable, seeded from YAML, applied live.
	defaults := director.Settings{
		Dataplane: director.DataplaneSettings{
			BatchSize: cfg.ClickHouse.BatchSize, FlushIntervalMs: cfg.ClickHouse.FlushIntervalMS,
			WriterWorkers: cfg.ClickHouse.WriterWorkers, MaxQueueRows: cfg.ClickHouse.MaxQueueRows,
			BackpressureMode: cfg.Pipeline.BackpressureMode, UnknownExporterMode: cfg.Security.UnknownExporterMode,
		},
		SkipRules: director.SkipRuleSettings{SkipDNS: cfg.Rules.SkipDNS, SkipPrivate: cfg.Rules.SkipPrivateToPrivate, SkipZero: cfg.Rules.SkipZeroBytes},
		Retention: director.RetentionSettings{Days: retDays},
		S3: director.S3Settings{
			Enabled: cfg.S3.Bucket != "", Endpoint: cfg.S3.Endpoint, Region: cfg.S3.Region, Bucket: cfg.S3.Bucket,
			AccessKey: cfg.S3.AccessKey, SecretKey: cfg.S3.SecretKey, PathPrefix: cfg.S3.PathPrefix, ExportFormat: csvOr(cfg.S3.ExportFormat),
		},
	}
	dirSrv.InitSettings(ctxBg, defaults)

	var lastS3 director.S3Settings
	lastRet := -1
	var lastArchConn interface{ Close() error }
	apply := func(set director.Settings) {
		lv := cfg.Live()
		lv.BatchSize = set.Dataplane.BatchSize
		lv.FlushInterval = time.Duration(set.Dataplane.FlushIntervalMs) * time.Millisecond
		if bp, e := config.ParseBackpressure(set.Dataplane.BackpressureMode); e == nil {
			lv.Backpressure = bp
		}
		if um, e := devreg.ParseUnknownMode(set.Dataplane.UnknownExporterMode); e == nil {
			lv.UnknownMode = um
		}
		lv.Rules = rules.RuleSet{SkipDNS: set.SkipRules.SkipDNS, SkipPrivateToPrivate: set.SkipRules.SkipPrivate, SkipZeroBytes: set.SkipRules.SkipZero}
		live.Store(lv)
		manager.Reload(set.Dataplane.WriterWorkers, set.Dataplane.MaxQueueRows)
		dirSrv.SetRetentionDays(set.Retention.Days)
		if fr != nil && set.Retention.Days != lastRet { // ALTER TTL only when it changes
			lastRet = set.Retention.Days
			tctx, tc := context.WithTimeout(context.Background(), 30*time.Second)
			if e := fr.SetTTLDays(tctx, set.Retention.Days); e != nil {
				log.Warn("apply retention TTL failed", "error", e)
			}
			tc()
		}
		if set.S3 != lastS3 { // rebuild the archive client only when S3 settings change
			lastS3 = set.S3
			if lastArchConn != nil { // close the previous archive ClickHouse pool (no leak)
				_ = lastArchConn.Close()
				lastArchConn = nil
			}
			if set.S3.Enabled && set.S3.Bucket != "" {
				if s3c, e := s3.New(s3.Config{Endpoint: set.S3.Endpoint, Region: set.S3.Region, Bucket: set.S3.Bucket, AccessKey: set.S3.AccessKey, SecretKey: set.S3.SecretKey, PathPrefix: set.S3.PathPrefix}); e != nil {
					log.Warn("S3 archive disabled: client init failed", "error", e)
					dirSrv.SetArchive(nil, "", "")
				} else if aconn, e := chwriter.Connect(cfg.ClickHouse); e != nil {
					log.Warn("S3 archive disabled: clickhouse connect failed", "error", e)
					dirSrv.SetArchive(nil, "", "")
				} else {
					lastArchConn = aconn
					ebctx, ebc := context.WithTimeout(context.Background(), 10*time.Second)
					if eb := s3c.EnsureBucket(ebctx); eb != nil {
						log.Warn("S3 archive: could not ensure bucket (uploads may fail)", "bucket", set.S3.Bucket, "error", eb)
					}
					ebc()
					dirSrv.SetArchive(archive.New(aconn, s3c, cfg.ClickHouse.Database, "flow_logs", m, log), set.S3.Bucket, csvOr(set.S3.ExportFormat))
					log.Info("S3 cold-archive enabled", "bucket", set.S3.Bucket)
				}
			} else {
				dirSrv.SetArchive(nil, "", "")
			}
		}
	}
	dirSrv.SetApplier(apply)
	apply(dirSrv.CurrentSettings()) // push seeded/persisted settings to the dataplane now

	// Dataplane stats snapshot for the Overview / Dataplanes pages.
	dirSrv.SetStats(func() director.DPStats {
		dp := dirSrv.CurrentSettings().Dataplane
		// QueueSize is the AGGREGATE depth across all writer shards, so the
		// capacity must also be aggregate (per-worker × workers).
		return director.DPStats{
			Ingested:     uint64(testutil.ToFloat64(m.FlowsDecoded)),
			Skipped:      uint64(testutil.ToFloat64(m.FlowsSkipped)),
			Inserted:     uint64(testutil.ToFloat64(m.FlowsInserted)),
			ArchiveBytes: uint64(testutil.ToFloat64(m.ArchiveBytes)),
			QueueSize:    int(testutil.ToFloat64(m.QueueSize)),
			QueueMax:     dp.MaxQueueRows * dp.WriterWorkers,
			Collectors:   1,
			Name:         cfg.Server.Name,
		}
	})

	// In-process registry: the collector pulls its device registry directly from
	// the Director store (no HTTP/token), applying via the managed hot-reload.
	managedCtx, managedCancel := context.WithCancel(context.Background())
	defer managedCancel()
	mc := managed.NewWithSource(managed.SourceFunc(dirSrv.Bundle), devices, live, cfg.Live().Rules, m, log)
	pctx, pcancel := context.WithTimeout(context.Background(), 10*time.Second)
	if perr := mc.PollOnce(pctx); perr != nil {
		log.Warn("initial registry load from control plane failed; starting empty", "error", perr)
	}
	pcancel()
	go mc.Run(managedCtx, time.Duration(cp.RegistryRefreshS)*time.Second)

	// ---- receivers ----
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
		rcv, rerr := receiver.New(b.name, cfg.Receiver.BindIP, b.port, cfg.Receiver.Workers, cfg.Receiver.UDPReadBufferMB, p, m, log)
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

	// ---- control-plane HTTP server ----
	if !cp.CookieSecure {
		log.Warn("cookie_secure=false: session cookies sent over plaintext HTTP; set cookie_secure: true behind TLS")
	}
	hs := &http.Server{Addr: cp.Bind, Handler: dirSrv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Info("control plane (console + API) listening", "bind", cp.Bind)
		if e := hs.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			srvErr <- e
		}
	}()

	// ---- auto-archival scheduler ----
	// Periodically moves hot days older than the configured window to S3 and drops
	// them from hot storage. No-op unless S3 + auto-archive are enabled in Settings.
	go func() {
		first := time.NewTimer(30 * time.Second)
		defer first.Stop()
		tick := time.NewTicker(6 * time.Hour)
		defer tick.Stop()
		sweep := func() {
			sctx, sc := context.WithTimeout(managedCtx, 30*time.Minute)
			defer sc()
			if days, rows, _, err := dirSrv.ArchiveSweep(sctx); err != nil {
				log.Error("auto-archive sweep failed", "error", err)
			} else if days > 0 {
				log.Info("auto-archive sweep done", "days", days, "rows", rows)
			}
		}
		for {
			select {
			case <-managedCtx.Done():
				return
			case <-first.C:
				sweep()
			case <-tick.C:
				sweep()
			}
		}
	}()

	log.Info("natlog running; SIGINT/SIGTERM to stop",
		"receivers", len(receivers), "metrics", cfg.Metrics.Bind)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received")
	case e := <-srvErr:
		log.Error("server failed", "error", e)
	}

	// ---- graceful shutdown ----
	managedCancel()
	for _, r := range receivers {
		r.Stop()
	}
	manager.Stop()
	log.Info("dataplane drained")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
	if fr != nil {
		_ = fr.Close()
	}
	if lastArchConn != nil {
		_ = lastArchConn.Close()
	}
	log.Info("shutdown complete")
	return nil
}

// resolveCP applies defaults and validates the control-plane config.
func resolveCP(cp config.CPConfig) (config.CPConfig, error) {
	if cp.Bind == "" {
		cp.Bind = "0.0.0.0:8080"
	}
	if cp.RegistryRefreshS <= 0 {
		cp.RegistryRefreshS = 15
	}
	if len(cp.SessionKey) < 16 {
		return cp, errors.New("cp.session_key must be at least 16 characters")
	}
	if cp.MySQLDSN == "" {
		return cp, errors.New("cp.mysql_dsn is required")
	}
	return cp, nil
}
