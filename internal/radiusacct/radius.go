// Package radiusacct implements authenticated RADIUS Accounting-Request intake
// and an IP-to-subscriber session cache for zero-API enrichment.
package radiusacct

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sync"
	"time"
)

type Session struct {
	Username, SessionID, NASIP string
	UpdatedAt                  time.Time
}
type Cache struct {
	mu   sync.RWMutex
	byIP map[string]Session
	ttl  time.Duration
}

func NewCache(ttl time.Duration) *Cache {
	if ttl <= 0 {
		ttl = 48 * time.Hour
	}
	return &Cache{byIP: map[string]Session{}, ttl: ttl}
}
func (c *Cache) Lookup(ip net.IP) string {
	if ip == nil {
		return ""
	}
	k := ip.String()
	c.mu.RLock()
	s, ok := c.byIP[k]
	c.mu.RUnlock()
	if !ok {
		return ""
	}
	if time.Since(s.UpdatedAt) > c.ttl {
		c.mu.Lock()
		delete(c.byIP, k)
		c.mu.Unlock()
		return ""
	}
	return s.Username
}
func (c *Cache) apply(ip net.IP, s Session, status uint32) {
	if ip == nil {
		return
	}
	k := ip.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if status == 2 {
		if old, ok := c.byIP[k]; !ok || s.SessionID == "" || old.SessionID == s.SessionID {
			delete(c.byIP, k)
		}
	} else if status == 1 || status == 3 {
		s.UpdatedAt = time.Now().UTC()
		c.byIP[k] = s
	}
}

type Server struct {
	conn   *net.UDPConn
	secret []byte
	cache  *Cache
	log    *slog.Logger
	wg     sync.WaitGroup
}

func New(bindIP string, port int, secret string, cache *Cache, log *slog.Logger) (*Server, error) {
	c, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(bindIP), Port: port})
	if e != nil {
		return nil, e
	}
	return &Server{conn: c, secret: []byte(secret), cache: cache, log: log.With("component", "radius-accounting")}, nil
}
func (s *Server) Start(ctx context.Context) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		buf := make([]byte, 65535)
		for {
			n, a, e := s.conn.ReadFromUDP(buf)
			if e != nil {
				return
			}
			resp, e := s.handle(buf[:n], a.IP)
			if e != nil {
				s.log.Warn("rejected accounting packet", "nas", a.IP, "error", e)
				continue
			}
			_, _ = s.conn.WriteToUDP(resp, a)
		}
	}()
	go func() { <-ctx.Done(); _ = s.conn.Close() }()
}
func (s *Server) Stop() { _ = s.conn.Close(); s.wg.Wait() }
func (s *Server) handle(p []byte, nas net.IP) ([]byte, error) {
	if len(p) < 20 || p[0] != 4 {
		return nil, errors.New("not an Accounting-Request")
	}
	n := int(binary.BigEndian.Uint16(p[2:4]))
	if n < 20 || n > len(p) {
		return nil, errors.New("invalid packet length")
	}
	p = p[:n]
	check := append([]byte(nil), p...)
	clear(check[4:20])
	check = append(check, s.secret...)
	sum := md5.Sum(check)
	if !bytes.Equal(sum[:], p[4:20]) {
		return nil, errors.New("invalid request authenticator")
	}
	var user, sid string
	var ip net.IP
	var status uint32
	for pos := 20; pos+2 <= len(p); {
		l := int(p[pos+1])
		if l < 2 || pos+l > len(p) {
			return nil, errors.New("invalid attribute")
		}
		v := p[pos+2 : pos+l]
		switch p[pos] {
		case 1:
			user = string(v)
		case 8:
			if len(v) == 4 {
				ip = net.IPv4(v[0], v[1], v[2], v[3])
			}
		case 40:
			if len(v) == 4 {
				status = binary.BigEndian.Uint32(v)
			}
		case 44:
			sid = string(v)
		}
		pos += l
	}
	if user == "" || ip == nil || status == 0 {
		return nil, errors.New("missing required accounting attributes")
	}
	s.cache.apply(ip, Session{Username: user, SessionID: sid, NASIP: nas.String()}, status)
	resp := make([]byte, 20)
	resp[0] = 5
	resp[1] = p[1]
	binary.BigEndian.PutUint16(resp[2:4], 20)
	h := md5.New()
	h.Write(resp[:4])
	h.Write(p[4:20])
	h.Write(s.secret)
	copy(resp[4:], h.Sum(nil))
	return resp, nil
}
