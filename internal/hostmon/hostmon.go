// Package hostmon observes host-level evidence-loss risks that application
// counters cannot see: kernel UDP queue overflow, clock skew and disk pressure.
package hostmon

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/natflow/natflow-dataplane/internal/metrics"
)

// UDPReceiveErrors returns Linux's host-wide Udp:RcvbufErrors counter.
func UDPReceiveErrors(r io.Reader) (uint64, error) {
	s := bufio.NewScanner(r)
	var names []string
	for s.Scan() {
		line := strings.Fields(s.Text())
		if len(line) == 0 || line[0] != "Udp:" {
			continue
		}
		if names == nil {
			names = line[1:]
			continue
		}
		for i, name := range names {
			if name == "RcvbufErrors" && i+1 < len(line) {
				return strconv.ParseUint(line[i+1], 10, 64)
			}
		}
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("Udp:RcvbufErrors not found")
}

func ReadUDPReceiveErrors(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return UDPReceiveErrors(f)
}

// DiskUsage returns the percentage used on the filesystem containing path.
func DiskUsage(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 0, errors.New("filesystem reports zero blocks")
	}
	return 100 * float64(st.Blocks-st.Bavail) / float64(st.Blocks), nil
}

// NTPOffset performs a minimal RFC 5905 client query. A positive result means
// the remote clock is ahead of the local clock.
func NTPOffset(ctx context.Context, server string) (time.Duration, error) {
	if !strings.Contains(server, ":") {
		server += ":123"
	}
	d := net.Dialer{}
	c, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	deadline, ok := ctx.Deadline()
	if ok {
		_ = c.SetDeadline(deadline)
	}
	p := make([]byte, 48)
	p[0] = 0x23
	t1 := time.Now()
	putNTP(p[40:], t1)
	if _, err = c.Write(p); err != nil {
		return 0, err
	}
	if _, err = io.ReadFull(c, p); err != nil {
		return 0, err
	}
	t4 := time.Now()
	if p[0]&7 != 4 || p[1] == 0 {
		return 0, errors.New("invalid/unsynchronised NTP response")
	}
	t2, err := parseNTP(p[32:40])
	if err != nil {
		return 0, err
	}
	t3, err := parseNTP(p[40:48])
	if err != nil {
		return 0, err
	}
	return (t2.Sub(t1) + t3.Sub(t4)) / 2, nil
}

const ntpEpoch = 2208988800

func putNTP(dst []byte, t time.Time) {
	sec := uint64(t.Unix() + ntpEpoch)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	binary.BigEndian.PutUint32(dst, uint32(sec))
	binary.BigEndian.PutUint32(dst[4:], uint32(frac))
}
func parseNTP(src []byte) (time.Time, error) {
	if len(src) < 8 {
		return time.Time{}, io.ErrUnexpectedEOF
	}
	sec := binary.BigEndian.Uint32(src)
	frac := binary.BigEndian.Uint32(src[4:])
	if sec == 0 {
		return time.Time{}, errors.New("zero NTP timestamp")
	}
	return time.Unix(int64(sec)-ntpEpoch, int64((uint64(frac)*1e9)>>32)), nil
}

type Snapshot struct {
	UDPDrops   uint64
	NTPOffset  time.Duration
	NTPHealthy bool
	DiskUsed   float64
	DiskLevel  int
}

// Sample updates Prometheus metrics and returns a snapshot. Individual source
// failures are returned together while successful observations are retained.
func Sample(ctx context.Context, m *metrics.Metrics, procPath, ntpServer, diskPath string, maxSkew time.Duration, previous *uint64) (Snapshot, error) {
	var out Snapshot
	var errs []error
	if n, err := ReadUDPReceiveErrors(procPath); err != nil {
		errs = append(errs, fmt.Errorf("udp drops: %w", err))
	} else {
		out.UDPDrops = n
		m.KernelUDPDropTotal.Set(float64(n))
		if previous != nil {
			if n >= *previous {
				m.KernelUDPReceiveErrors.Add(float64(n - *previous))
			}
			*previous = n
		}
	}
	if ntpServer != "" {
		if off, err := NTPOffset(ctx, ntpServer); err != nil {
			errs = append(errs, fmt.Errorf("ntp: %w", err))
			m.NTPClockHealthy.Set(0)
		} else {
			out.NTPOffset = off
			out.NTPHealthy = math.Abs(off.Seconds()) <= maxSkew.Seconds()
			m.NTPClockOffsetSeconds.Set(off.Seconds())
			if out.NTPHealthy {
				m.NTPClockHealthy.Set(1)
			} else {
				m.NTPClockHealthy.Set(0)
			}
		}
	}
	if diskPath != "" {
		if used, err := DiskUsage(diskPath); err != nil {
			errs = append(errs, fmt.Errorf("disk: %w", err))
		} else {
			out.DiskUsed = used
			if used >= 90 {
				out.DiskLevel = 2
			} else if used >= 85 {
				out.DiskLevel = 1
			}
			m.DiskUsedPercent.Set(used)
			m.DiskPressureLevel.Set(float64(out.DiskLevel))
		}
	}
	return out, errors.Join(errs...)
}

type Config struct {
	ProcPath, NTPServer, DiskPath string
	MaxSkew, Interval             time.Duration
	AlertPercent, SafetyPercent   float64
}

// Run performs an immediate startup check, then repeats until ctx is canceled.
// onPressure is invoked on transitions into warning/safety states.
func Run(ctx context.Context, cfg Config, m *metrics.Metrics, log *slog.Logger, onPressure func(Snapshot)) {
	if cfg.ProcPath == "" {
		cfg.ProcPath = "/proc/net/snmp"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Minute
	}
	if cfg.MaxSkew <= 0 {
		cfg.MaxSkew = 500 * time.Millisecond
	}
	if cfg.AlertPercent <= 0 {
		cfg.AlertPercent = 85
	}
	if cfg.SafetyPercent <= 0 {
		cfg.SafetyPercent = 90
	}
	var previous uint64
	previousSet := false
	if n, err := ReadUDPReceiveErrors(cfg.ProcPath); err == nil {
		previous = n
		previousSet = true
	}
	lastLevel := -1
	check := func() {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		snap, err := Sample(qctx, m, cfg.ProcPath, cfg.NTPServer, cfg.DiskPath, cfg.MaxSkew, func() *uint64 {
			if previousSet {
				return &previous
			}
			return nil
		}())
		if !previousSet && err == nil {
			previous = snap.UDPDrops
			previousSet = true
		}
		snap.DiskLevel = 0
		if snap.DiskUsed >= cfg.SafetyPercent {
			snap.DiskLevel = 2
		} else if snap.DiskUsed >= cfg.AlertPercent {
			snap.DiskLevel = 1
		}
		m.DiskPressureLevel.Set(float64(snap.DiskLevel))
		if err != nil {
			log.Warn("host safeguard check incomplete", "error", err)
		}
		if cfg.NTPServer != "" && !snap.NTPHealthy {
			log.Error("collector clock outside NTP policy or check failed", "offset", snap.NTPOffset, "max_skew", cfg.MaxSkew)
		}
		if snap.DiskLevel > 0 && snap.DiskLevel != lastLevel {
			log.Error("ClickHouse disk pressure threshold reached", "used_percent", snap.DiskUsed, "level", snap.DiskLevel)
			if onPressure != nil {
				onPressure(snap)
			}
		}
		lastLevel = snap.DiskLevel
	}
	check()
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}
