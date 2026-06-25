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
	TemplatesReceived  prometheus.Counter // NetFlow v9/IPFIX templates parsed
	TemplateUnknown    prometheus.Counter // data flowsets referencing an unknown template
	QueueSize          prometheus.Gauge   // aggregate writer queue depth

	// Runtime config reload.
	ConfigReloads      prometheus.Counter
	ConfigReloadErrors prometheus.Counter

	// Sharded writer pool.
	WriterBatches       prometheus.Counter
	WriterBatchRows     prometheus.Counter
	WriterRetries       prometheus.Counter
	WriterInsertLatency prometheus.Histogram
	WriterQueueSize     *prometheus.GaugeVec   // labeled by worker
	WriterQueueDropped  *prometheus.CounterVec // labeled by worker

	// Archive.
	ArchiveRuns          prometheus.Counter
	ArchiveRows          prometheus.Counter
	ArchiveBytes         prometheus.Counter
	ArchiveErrors        prometheus.Counter
	ArchiveUploadLatency prometheus.Histogram

	// Device registry.
	DeviceRegistryEntries      prometheus.Gauge
	DeviceMatchedPackets       prometheus.Counter
	DeviceRejectedPackets      prometheus.Counter
	UnknownExporterPackets     prometheus.Counter
	UnknownExporterFlows       prometheus.Counter
	DeviceRuleSkipped          prometheus.Counter
	DeviceRegistryReloads      prometheus.Counter
	DeviceRegistryReloadErrors prometheus.Counter

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
		TemplatesReceived:  counter("templates_received_total", "NetFlow v9/IPFIX templates parsed from exporters."),
		TemplateUnknown:    counter("template_unknown_total", "Data flowsets dropped because their template was not yet known."),
	}
	m.QueueSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_queue_size",
		Help: "Flow records currently buffered across all writer queues.",
	})
	reg.MustRegister(m.QueueSize)

	m.ConfigReloads = counter("config_reloads_total", "Successful runtime config reloads.")
	m.ConfigReloadErrors = counter("config_reload_errors_total", "Failed runtime config reloads.")

	m.WriterBatches = counter("writer_batches_total", "Batches sent to ClickHouse across all writers.")
	m.WriterBatchRows = counter("writer_batch_rows_total", "Rows sent to ClickHouse in batches.")
	m.WriterRetries = counter("writer_retries_total", "Writer batch send retries.")
	m.WriterInsertLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "writer_insert_latency_ms",
		Help:    "ClickHouse batch insert latency in milliseconds.",
		Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000},
	})
	reg.MustRegister(m.WriterInsertLatency)
	m.WriterQueueSize = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "writer_queue_size",
		Help: "Flow records currently buffered in each writer queue.",
	}, []string{"worker"})
	reg.MustRegister(m.WriterQueueSize)
	m.WriterQueueDropped = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "writer_queue_dropped_total",
		Help: "Flow records dropped by each writer queue due to backpressure.",
	}, []string{"worker"})
	reg.MustRegister(m.WriterQueueDropped)

	m.ArchiveRuns = counter("archive_runs_total", "Archive export runs.")
	m.ArchiveRows = counter("archive_rows_total", "Rows exported by archive runs.")
	m.ArchiveBytes = counter("archive_bytes_total", "Bytes uploaded by archive runs.")
	m.ArchiveErrors = counter("archive_errors_total", "Archive export/upload failures.")
	m.ArchiveUploadLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "archive_upload_latency_ms",
		Help:    "Archive upload latency in milliseconds.",
		Buckets: []float64{10, 50, 100, 250, 500, 1000, 5000, 30000, 120000},
	})
	reg.MustRegister(m.ArchiveUploadLatency)

	m.DeviceRegistryEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "device_registry_entries",
		Help: "Number of devices in the registry.",
	})
	reg.MustRegister(m.DeviceRegistryEntries)
	m.DeviceMatchedPackets = counter("device_matched_packets_total", "Packets from a registered, enabled device.")
	m.DeviceRejectedPackets = counter("device_rejected_packets_total", "Packets dropped by device policy (disabled device or unknown-in-reject-mode).")
	m.UnknownExporterPackets = counter("unknown_exporter_packets_total", "Packets from an exporter not in the registry.")
	m.UnknownExporterFlows = counter("unknown_exporter_flows_total", "Flows decoded from exporters not in the registry.")
	m.DeviceRuleSkipped = counter("device_rule_skipped_total", "Flows skipped by a matched device's rules.")
	m.DeviceRegistryReloads = counter("device_registry_reloads_total", "Successful device registry reloads.")
	m.DeviceRegistryReloadErrors = counter("device_registry_reload_errors_total", "Failed device registry reloads.")
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
