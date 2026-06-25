// Package metrics defines the Prometheus instrumentation exposed by the
// collector and a small HTTP server to serve it.
package metrics

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every counter/gauge the collector reports. All fields are
// safe for concurrent use.
type Metrics struct {
	PacketsReceived    prometheus.Counter
	PacketsDropped     prometheus.Counter
	PacketsUnsupported prometheus.Counter
	FlowsDecoded       prometheus.Counter
	FlowsSkipped       prometheus.Counter
	FlowsInserted      prometheus.Counter
	FlowsDropped       prometheus.Counter // dropped without insertion (queue full / shutdown)
	FlowsRejected      prometheus.Counter // rejected by ClickHouse on append
	InsertErrors       prometheus.Counter
	QueueSize          prometheus.Gauge

	reg *prometheus.Registry
}

// New constructs the metric set in a private registry (so multiple instances
// can coexist in tests).
func New() *Metrics {
	reg := prometheus.NewRegistry()
	counter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
		reg.MustRegister(c)
		return c
	}
	m := &Metrics{
		PacketsReceived:    counter("packets_received_total", "UDP datagrams received across all listeners."),
		PacketsDropped:     counter("packets_dropped_total", "UDP datagrams dropped due to decode or validation errors."),
		PacketsUnsupported: counter("packets_unsupported_total", "UDP datagrams for a recognized but not-yet-decoded protocol (v9/IPFIX in v1)."),
		FlowsDecoded:       counter("flows_decoded_total", "Flow records successfully decoded."),
		FlowsSkipped:       counter("flows_skipped_total", "Flow records dropped by skip rules."),
		FlowsInserted:      counter("flows_inserted_total", "Flow records inserted into ClickHouse."),
		FlowsDropped:       counter("flows_dropped_total", "Flow records dropped without insertion (writer queue full or shutdown deadline)."),
		FlowsRejected:      counter("flows_rejected_total", "Flow records rejected by ClickHouse during row append."),
		InsertErrors:       counter("insert_errors_total", "ClickHouse batch insert failures after retries."),
	}
	m.QueueSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_queue_size",
		Help: "Flow records currently buffered in the writer queue.",
	})
	reg.MustRegister(m.QueueSize)
	// Go runtime + process metrics are handy for ops dashboards.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m.reg = reg
	return m
}

// Handler returns an HTTP handler serving the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Server wraps the metrics HTTP server.
type Server struct {
	srv *http.Server
}

// NewServer builds a metrics server bound to addr, serving /metrics and a
// /healthz liveness probe.
func (m *Metrics) NewServer(addr string) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return &Server{srv: &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}}
}

// Start runs the server in a background goroutine; a fatal listen error (other
// than a clean shutdown) is delivered on errc.
func (s *Server) Start(errc chan<- error) {
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
