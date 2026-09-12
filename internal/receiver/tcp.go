package receiver

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/natflow/natflow-dataplane/internal/metrics"
)

// TCPReceiver accepts newline-delimited RFC 6587 non-transparent syslog.
type TCPReceiver struct {
	name    string
	ln      net.Listener
	handler Handler
	metrics *metrics.Metrics
	log     *slog.Logger
	wg      sync.WaitGroup
	stop    sync.Once
	mu      sync.Mutex
	active  map[net.Conn]struct{}
}

func NewTCP(name, bindIP string, port int, h Handler, m *metrics.Metrics, log *slog.Logger) (*TCPReceiver, error) {
	ln, e := net.Listen("tcp", net.JoinHostPort(bindIP, fmt.Sprint(port)))
	if e != nil {
		return nil, e
	}
	return &TCPReceiver{name: name, ln: ln, handler: h, metrics: m, log: log.With("listener", name), active: map[net.Conn]struct{}{}}, nil
}
func (r *TCPReceiver) Start() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		for {
			c, e := r.ln.Accept()
			if e != nil {
				if errors.Is(e, net.ErrClosed) {
					return
				}
				continue
			}
			r.wg.Add(1)
			r.mu.Lock()
			r.active[c] = struct{}{}
			r.mu.Unlock()
			go r.read(c)
		}
	}()
}
func (r *TCPReceiver) read(c net.Conn) {
	defer r.wg.Done()
	defer func() { r.mu.Lock(); delete(r.active, c); r.mu.Unlock(); _ = c.Close() }()
	ip := net.IP(nil)
	if a, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ip = a.IP
	}
	s := bufio.NewScanner(c)
	buf := make([]byte, 64*1024)
	s.Buffer(buf, 1024*1024)
	for s.Scan() {
		b := append([]byte(nil), s.Bytes()...)
		r.metrics.PacketsReceived.Inc()
		r.handler.HandlePacket(b, ip)
	}
}
func (r *TCPReceiver) Stop() {
	r.stop.Do(func() {
		_ = r.ln.Close()
		r.mu.Lock()
		for c := range r.active {
			_ = c.Close()
		}
		r.mu.Unlock()
	})
	r.wg.Wait()
}
