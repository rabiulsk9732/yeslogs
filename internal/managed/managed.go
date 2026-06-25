// Package managed implements the collector side of control-plane management: it
// fetches the device registry + exporter policy from a Source (the Director,
// over HTTP for split deployments, or in-process when DP and CP run as one
// service) and applies it through the existing atomic device/Live stores — the
// same hot-reload path used for local config, so the dataplane is fully driven
// from the control plane.
package managed

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/director/agentcfg"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/rules"
)

// Source supplies the current config bundle. Implementations: HTTP (split
// deployment) and in-process (single unified service).
type Source interface {
	Fetch(ctx context.Context) (agentcfg.Bundle, error)
}

// httpSource pulls the bundle from a Director over HTTP.
type httpSource struct {
	url   string
	token string
	httpc *http.Client
}

func (h httpSource) Fetch(ctx context.Context) (agentcfg.Bundle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	if err != nil {
		return agentcfg.Bundle{}, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	resp, err := h.httpc.Do(req)
	if err != nil {
		return agentcfg.Bundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return agentcfg.Bundle{}, fmt.Errorf("director returned status %d", resp.StatusCode)
	}
	var b agentcfg.Bundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return agentcfg.Bundle{}, fmt.Errorf("decode bundle: %w", err)
	}
	return b, nil
}

// SourceFunc adapts a function to a Source (used for the in-process Director).
type SourceFunc func(ctx context.Context) (agentcfg.Bundle, error)

func (f SourceFunc) Fetch(ctx context.Context) (agentcfg.Bundle, error) { return f(ctx) }

// Client fetches config from a Source and applies it.
type Client struct {
	src       Source
	devices   *device.Store
	live      *config.Store
	metrics   *metrics.Metrics
	log       *slog.Logger
	baseRules rules.RuleSet

	mu          sync.Mutex
	lastVersion string
}

// New builds a Client that pulls from a Director over HTTP.
func New(dir config.DirectorConfig, devices *device.Store, live *config.Store, base rules.RuleSet, m *metrics.Metrics, log *slog.Logger) *Client {
	src := httpSource{
		url:   strings.TrimRight(dir.URL, "/") + "/api/v1/agent/config",
		token: dir.Token,
		httpc: &http.Client{Timeout: 10 * time.Second},
	}
	return NewWithSource(src, devices, live, base, m, log)
}

// NewWithSource builds a Client backed by an arbitrary Source (e.g. in-process).
func NewWithSource(src Source, devices *device.Store, live *config.Store, base rules.RuleSet, m *metrics.Metrics, log *slog.Logger) *Client {
	return &Client{
		src:       src,
		devices:   devices,
		live:      live,
		metrics:   m,
		log:       log.With("component", "managed"),
		baseRules: base,
	}
}

// PollOnce fetches the config bundle and applies it atomically. On any error the
// current registry is kept unchanged.
func (c *Client) PollOnce(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	b, err := c.src.Fetch(ctx)
	if err != nil {
		return err
	}

	specs := make([]device.Spec, 0, len(b.Devices))
	for _, d := range b.Devices {
		sd, sp, sz := d.SkipDNS, d.SkipPrivate, d.SkipZero
		specs = append(specs, device.Spec{
			Name: d.Name, Enabled: d.Enabled, ExporterIP: d.ExporterIP,
			ISPID: d.ISPID, DeviceID: d.DeviceID, Protocol: d.Protocol, Profile: d.Profile,
			Rules: &device.RuleSpec{SkipDNS: &sd, SkipPrivateToPrivate: &sp, SkipZeroBytes: &sz},
		})
	}
	reg, err := device.Build(specs, c.baseRules)
	if err != nil {
		return fmt.Errorf("build registry from director: %w", err)
	}
	c.devices.Store(reg)

	mode, err := device.ParseUnknownMode(b.UnknownExporterMode)
	if err != nil {
		mode = device.ModeReject // managed default: only registered exporters
	}
	cur := *c.live.Load()
	cur.UnknownMode = mode
	c.live.Store(cur)

	if c.metrics != nil {
		c.metrics.DeviceRegistryEntries.Set(float64(reg.Len()))
	}
	if b.Version != c.lastVersion {
		c.log.Info("applied director config", "version", b.Version, "devices", reg.Len(), "unknown_mode", mode.String())
		c.lastVersion = b.Version
	}
	return nil
}

// Run polls the Source every interval until ctx is cancelled.
func (c *Client) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.PollOnce(ctx); err != nil {
				c.log.Warn("config poll failed; keeping current registry", "error", err)
			}
		}
	}
}
