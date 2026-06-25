// Command director is the NATFlow control-plane service (multi-tenant ISP/device
// management, agent config API, flow dashboards).
//
// Bootstrap:
//
//	director --config director.yaml --migrate
//	director --config director.yaml --create-admin --email a@b.com --password ...
//	director --config director.yaml --create-agent --name dp-india-01   # prints token once
//	director --config director.yaml                                     # run server
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

	"gopkg.in/yaml.v3"

	"github.com/natflow/natflow-dataplane/internal/director"
	"github.com/natflow/natflow-dataplane/internal/director/store"
	"github.com/natflow/natflow-dataplane/internal/logger"
)

type config struct {
	Bind         string `yaml:"bind"`
	SessionKey   string `yaml:"session_key"`
	CookieSecure bool   `yaml:"cookie_secure"`
	MySQLDSN     string `yaml:"mysql_dsn"`
	ClickHouse   struct {
		Addr     string `yaml:"addr"`
		Database string `yaml:"database"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"clickhouse"`
	FlowDays int `yaml:"flow_days"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "director:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "director.yaml", "path to the YAML config")
	migrate := flag.Bool("migrate", false, "run schema migration and exit")
	createAdmin := flag.Bool("create-admin", false, "create a director admin and exit")
	createAgent := flag.Bool("create-agent", false, "create an agent (prints token once) and exit")
	email := flag.String("email", "", "admin email (with --create-admin)")
	password := flag.String("password", "", "admin password (with --create-admin)")
	name := flag.String("name", "", "agent name (with --create-agent)")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	log, _, closeLog, err := logger.New("info", "")
	if err != nil {
		return err
	}
	defer func() { _ = closeLog.Close() }()

	st, err := store.OpenMySQL(cfg.MySQLDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()

	switch {
	case *migrate:
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		fmt.Println("migration complete")
		return nil
	case *createAdmin:
		if *email == "" || *password == "" {
			return fmt.Errorf("--create-admin needs --email and --password")
		}
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		hash, err := director.HashPassword(*password)
		if err != nil {
			return err
		}
		if _, err := st.CreateUser(ctx, store.User{Email: strings.ToLower(*email), PasswordHash: hash, Role: store.RoleDirector}); err != nil {
			return fmt.Errorf("create admin: %w", err)
		}
		fmt.Printf("created director admin %s\n", *email)
		return nil
	case *createAgent:
		if *name == "" {
			return fmt.Errorf("--create-agent needs --name")
		}
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		tok, err := director.NewToken()
		if err != nil {
			return err
		}
		if _, err := st.CreateAgent(ctx, *name, director.HashToken(tok)); err != nil {
			return fmt.Errorf("create agent: %w", err)
		}
		fmt.Printf("created agent %q. Token (store it now, shown once):\n%s\n", *name, tok)
		return nil
	}

	// Run the server.
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if !cfg.CookieSecure {
		log.Warn("cookie_secure=false: session cookies are sent over plaintext HTTP and can be stolen via MITM; set cookie_secure: true when serving over HTTPS")
	}

	var fr *director.FlowReader
	if cfg.ClickHouse.Addr != "" {
		fr, err = director.NewFlowReader(cfg.ClickHouse.Addr, cfg.ClickHouse.Database, cfg.ClickHouse.Username, cfg.ClickHouse.Password)
		if err != nil {
			log.Warn("flow dashboard disabled: clickhouse unavailable", "error", err)
			fr = nil
		} else {
			defer fr.Close()
		}
	}

	srv, err := director.New(director.Config{
		SessionKey:   []byte(cfg.SessionKey),
		CookieSecure: cfg.CookieSecure,
		FlowDays:     cfg.FlowDays,
	}, st, fr, log)
	if err != nil {
		return err
	}

	hs := &http.Server{Addr: cfg.Bind, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() {
		log.Info("director listening", "bind", cfg.Bind, "clickhouse", cfg.ClickHouse.Addr != "")
		if e := hs.ListenAndServe(); e != nil && !errors.Is(e, http.ErrServerClosed) {
			errc <- e
		}
	}()

	sctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-sctx.Done():
		log.Info("shutdown signal")
	case e := <-errc:
		return e
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(shutCtx)
}

func loadConfig(path string) (*config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Bind == "" {
		c.Bind = "127.0.0.1:8080"
	}
	if len(c.SessionKey) < 16 {
		return nil, fmt.Errorf("session_key must be at least 16 characters")
	}
	if c.MySQLDSN == "" {
		return nil, fmt.Errorf("mysql_dsn is required")
	}
	return &c, nil
}
