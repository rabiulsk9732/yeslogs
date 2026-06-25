// Package pipeline wires a protocol decoder to the normalizer, skip rules and
// writer. One Pipeline is created per protocol/port; all share the normalizer,
// rules and writer.
package pipeline

import (
	"errors"
	"log/slog"
	"net"
	"sync"

	"github.com/natflow/natflow-dataplane/internal/decoder"
	"github.com/natflow/natflow-dataplane/internal/metrics"
	"github.com/natflow/natflow-dataplane/internal/normalizer"
	"github.com/natflow/natflow-dataplane/internal/rules"
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
	dec     decoder.Decoder
	norm    *normalizer.Normalizer
	rules   *rules.RuleSet
	writer  Writer
	metrics *metrics.Metrics
	log     *slog.Logger
	kind    string

	// pool recycles decode scratch buffers across the concurrent HandlePacket
	// calls so steady-state decoding allocates nothing per packet.
	pool sync.Pool
}

// New builds a Pipeline for dec.
func New(dec decoder.Decoder, norm *normalizer.Normalizer, rs *rules.RuleSet, w Writer, m *metrics.Metrics, log *slog.Logger) *Pipeline {
	p := &Pipeline{
		dec:     dec,
		norm:    norm,
		rules:   rs,
		writer:  w,
		metrics: m,
		log:     log.With("component", "pipeline", "proto", dec.Kind()),
		kind:    dec.Kind(),
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
	bufp := p.pool.Get().(*[]decoder.Flow)
	flows := (*bufp)[:0]

	flows, err := p.dec.Decode(flows, payload, exporter)
	if err != nil {
		// A recognized-but-undecodable protocol (v9/ipfix in v1) is counted
		// separately from genuinely malformed/erroring datagrams. Logged at
		// debug to avoid hot-path spam.
		if errors.Is(err, decoder.ErrUnsupported) {
			p.metrics.PacketsUnsupported.Inc()
		} else {
			p.metrics.PacketsDropped.Inc()
		}
		p.log.Debug("decode failed", "bytes", len(payload), "error", err)
		*bufp = flows
		p.pool.Put(bufp)
		return
	}

	for i := range flows {
		rec := p.norm.Normalize(flows[i], p.kind)
		p.metrics.FlowsDecoded.Inc()
		if skip, _ := p.rules.ShouldSkip(&rec); skip {
			p.metrics.FlowsSkipped.Inc()
			continue
		}
		if !p.writer.Enqueue(rec) {
			p.metrics.FlowsDropped.Inc()
		}
	}

	// Don't return a pathologically large scratch buffer to the pool (a hostile
	// packet could otherwise pin oversized arrays, one per worker); let it GC.
	if cap(flows) <= maxPooledFlows {
		*bufp = flows
		p.pool.Put(bufp)
	}
}
