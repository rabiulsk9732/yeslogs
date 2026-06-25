// Package receiver binds a UDP socket and fans incoming datagrams out to a
// Handler across a pool of reader goroutines.
package receiver

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/natflow/natflow-dataplane/internal/metrics"
)

// maxDatagram is the largest UDP payload we will read (theoretical IPv4 max).
const maxDatagram = 65535

// readErrorBackoff paces the worker after an unexpected read error so a
// persistent error condition cannot spin the CPU or flood the logs.
const readErrorBackoff = 50 * time.Millisecond

// Handler consumes a single UDP payload. Implementations must not retain
// payload after returning, because the buffer is reused for the next datagram.
type Handler interface {
	HandlePacket(payload []byte, exporter net.IP)
}

// Receiver reads datagrams from one UDP socket using a pool of workers. Go's
// netpoller makes concurrent ReadFromUDP on a single connection safe, so each
// worker reads independently into its own buffer.
type Receiver struct {
	name    string
	conn    *net.UDPConn
	workers int
	handler Handler
	metrics *metrics.Metrics
	log     *slog.Logger

	wg sync.WaitGroup
}

// New binds a UDP socket on bindIP:port. readBufMB sets the kernel receive
// buffer (SO_RCVBUF) to absorb traffic bursts.
func New(name, bindIP string, port, workers, readBufMB int, h Handler, m *metrics.Metrics, log *slog.Logger) (*Receiver, error) {
	addr := &net.UDPAddr{IP: net.ParseIP(bindIP), Port: port}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s udp %s:%d: %w", name, bindIP, port, err)
	}
	if readBufMB > 0 {
		if err := conn.SetReadBuffer(readBufMB * 1024 * 1024); err != nil {
			log.Warn("could not set UDP read buffer; kernel net.core.rmem_max may be too low",
				"listener", name, "requested_mb", readBufMB, "error", err)
		}
	}
	if workers <= 0 {
		workers = 1
	}
	return &Receiver{
		name:    name,
		conn:    conn,
		workers: workers,
		handler: h,
		metrics: m,
		log:     log.With("listener", name, "port", port),
	}, nil
}

// LocalAddr returns the address the listener is bound to. Useful for tests
// that bind to an ephemeral port (port 0).
func (r *Receiver) LocalAddr() net.Addr { return r.conn.LocalAddr() }

// Start launches the worker goroutines and returns immediately.
func (r *Receiver) Start() {
	r.log.Info("listener started", "workers", r.workers)
	for i := 0; i < r.workers; i++ {
		r.wg.Add(1)
		go r.worker()
	}
}

func (r *Receiver) worker() {
	defer r.wg.Done()
	buf := make([]byte, maxDatagram)
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // socket closed by Stop
			}
			r.log.Warn("udp read error", "error", err)
			time.Sleep(readErrorBackoff)
			continue
		}
		r.metrics.PacketsReceived.Inc()
		r.handler.HandlePacket(buf[:n], addr.IP)
	}
}

// Stop closes the socket (which unblocks the readers) and waits for the workers
// to exit.
func (r *Receiver) Stop() {
	_ = r.conn.Close()
	r.wg.Wait()
	r.log.Info("listener stopped")
}
