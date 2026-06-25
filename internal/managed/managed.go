// Package managed implements the collector side of control-plane management: it
// pulls the device registry + exporter policy from the Director and applies it
// through the existing atomic device/Live stores (the same hot-reload path used
// for local config), so the dataplane is fully driven from the control plane.
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

// Client pulls config from the Director and applies it.
type Client struct {
	url       string
	token     string
	httpc     *http.Client
	devices   *device.Store
	live      *config.Store
	metrics   *metrics.Metrics
	log       *slog.Logger
	baseRules rules.RuleSet

	mu          sync.Mutex
	lastVersion string
}

// New builds a managed Client.
func New(dir config.DirectorConfig, devices *device.Store, live *config.Store, base rules.RuleSet, m *metrics.Metrics, log *slog.Logger) *Client {
	return &Client{
		url:       strings.TrimRight(dir.URL, "/") + "/api/v1/agent/config",
		token:     dir.Token,
		httpc:     &http.Client{Timeout: 10 * time.Second},
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("director returned status %d", resp.StatusCode)
	}
	var b agentcfg.Bundle
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return fmt.Errorf("decode bundle: %w", err)
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

// Run polls the Director every interval until ctx is cancelled.
func (c *Client) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.PollOnce(ctx); err != nil {
				c.log.Warn("director poll failed; keeping current registry", "error", err)
			}
		}
	}
}
