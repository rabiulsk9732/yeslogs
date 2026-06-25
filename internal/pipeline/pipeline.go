// Package pipeline wires a protocol decoder to the device registry, normalizer,
// skip rules and writer. One Pipeline is created per protocol/port; all share
// the registry, normalizer, rules (via the live config) and writer.
package pipeline

import (
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/natflow/natflow-dataplane/internal/config"
	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/device"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

// maxPooledFlows caps the capacity of decode scratch buffers retained in the
// pool, so an oversized buffer grown by one large packet is not pinned.
const maxPooledFlows = 16384

// Writer is the subset of the ClickHouse writer the pipeline depends on.
type Writer interface {
	// Enqueue submits a record for insertion, returning false if it was
	// dropped because the queue is full.
	Enqueue(normalizer.FlowRecord) bool
}

// Pipeline processes raw UDP payloads for a single protocol. It implements
// receiver.Handler and is safe for concurrent use.
type Pipeline struct {
	dec       decoder.Decoder
	norm      *normalizer.Normalizer
	live      *config.Store
	devices   *device.Store
	writer    Writer
	metrics   *metrics.Metrics
	log       *slog.Logger
	kind      string
	defISPID  uint32
	defDevice uint32

	pool sync.Pool
}

// New builds a Pipeline for dec. Skip rules and the unknown-exporter mode are
// read live from the config store; device identity from the device registry.
// defISPID/defDevice are used for unknown exporters in "allow" mode.
func New(dec decoder.Decoder, norm *normalizer.Normalizer, live *config.Store, devices *device.Store, defISPID, defDevice uint32, w Writer, m *metrics.Metrics, log *slog.Logger) *Pipeline {
	p := &Pipeline{
		dec:       dec,
		norm:      norm,
		live:      live,
		devices:   devices,
		writer:    w,
		metrics:   m,
		log:       log.With("component", "pipeline", "proto", dec.Kind()),
		kind:      dec.Kind(),
		defISPID:  defISPID,
		defDevice: defDevice,
	}
	p.pool.New = func() any {
		s := make([]decoder.Flow, 0, 64)
		return &s
	}
	return p
}

// HandlePacket decodes one UDP payload and pushes the surviving flows to the
// writer. It is safe to call concurrently from multiple receiver workers.
func (p *Pipeline) HandlePacket(payload []byte, exporter net.IP) {
	live := p.live.Load()
	dev, matched := p.devices.Load().Lookup(exporter)

	// Resolve identity, rules and admission per the device registry / mode.
	var ispID, deviceID uint32
	var ruleSet = live.Rules
	matchedDevice := false

	switch {
	case matched && !dev.Enabled:
		p.metrics.DeviceRejectedPackets.Inc()
		return
	case matched:
		ispID, deviceID = dev.ISPID, dev.DeviceID
		ruleSet = dev.Rules
		matchedDevice = true
		p.metrics.DeviceMatchedPackets.Inc()
	default: // unregistered exporter
		switch live.UnknownMode {
		case device.ModeReject:
			p.metrics.UnknownExporterPackets.Inc()
			p.metrics.DeviceRejectedPackets.Inc()
			return
		case device.ModeObserve:
			p.metrics.UnknownExporterPackets.Inc()
			ispID, deviceID = 0, 0
		default: // ModeAllow
			p.metrics.UnknownExporterPackets.Inc()
			ispID, deviceID = p.defISPID, p.defDevice
		}
	}

	bufp := p.pool.Get().(*[]decoder.Flow)
	flows := (*bufp)[:0]

	flows, err := p.dec.Decode(flows, payload, exporter)
	if err != nil {
		if errors.Is(err, decoder.ErrUnsupported) {
			p.metrics.PacketsUnsupported.Inc()
		} else {
			p.metrics.PacketsDropped.Inc()
		}
		p.log.Debug("decode failed", "bytes", len(payload), "error", err)
		p.recycle(bufp, flows)
		return
	}

	for i := range flows {
		rec := p.norm.Normalize(flows[i], p.kind, ispID, deviceID)
		p.metrics.FlowsDecoded.Inc()
		if !matchedDevice {
			p.metrics.UnknownExporterFlows.Inc()
		}
		if skip, _ := ruleSet.ShouldSkip(&rec); skip {
			p.metrics.FlowsSkipped.Inc()
			if matchedDevice {
				p.metrics.DeviceRuleSkipped.Inc()
			}
			continue
		}
		if !p.writer.Enqueue(rec) {
			p.metrics.FlowsDropped.Inc()
		}
	}

	p.recycle(bufp, flows)
}

func (p *Pipeline) recycle(bufp *[]decoder.Flow, flows []decoder.Flow) {
	// Don't return a pathologically large scratch buffer to the pool (a hostile
	// packet could otherwise pin oversized arrays, one per worker); let it GC.
	if cap(flows) <= maxPooledFlows {
		*bufp = flows
		p.pool.Put(bufp)
	}
}
