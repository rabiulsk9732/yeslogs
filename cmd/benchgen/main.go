// Command benchgen is a NetFlow v5 load generator for benchmarking the
// collector. It emits valid v5 datagrams at a target packet rate, with a
// configurable mix of DNS / private-to-private / zero-byte flows so the skip
// rules are exercised under load, and prints throughput stats every second.
//
// Example:
//
//	benchgen --target 127.0.0.1:2055 --pps 10000 --flows-per-packet 30 \
//	         --duration 60s --dns-percent 30 --private-percent 20 --zero-byte-percent 5
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	v5HeaderLen = 24
	v5RecordLen = 48
	v5MaxFlows  = 30 // NetFlow v5 protocol maximum records per datagram
)

type config struct {
	target      string
	pps         int
	flowsPerPkt int
	duration    time.Duration
	dnsPct      int
	privatePct  int
	zeroBytePct int
	senders     int
	ringSize    int
}

// counters holds atomically-updated send statistics.
type counters struct {
	packets atomic.Int64
	flows   atomic.Int64
	bytes   atomic.Int64
	errors  atomic.Int64
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.target, "target", "127.0.0.1:2055", "collector UDP address host:port")
	flag.IntVar(&cfg.pps, "pps", 1000, "target packets per second (across all senders)")
	flag.IntVar(&cfg.flowsPerPkt, "flows-per-packet", 30, "flow records per packet (max 30)")
	flag.DurationVar(&cfg.duration, "duration", 30*time.Second, "how long to send")
	flag.IntVar(&cfg.dnsPct, "dns-percent", 0, "percent of flows that are DNS (dst port 53)")
	flag.IntVar(&cfg.privatePct, "private-percent", 0, "percent of flows that are private->private")
	flag.IntVar(&cfg.zeroBytePct, "zero-byte-percent", 0, "percent of flows with zero bytes")
	flag.IntVar(&cfg.senders, "senders", 1, "number of concurrent sender goroutines/sockets")
	flag.IntVar(&cfg.ringSize, "ring-size", 1024, "number of distinct pre-built packets to cycle")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "benchgen:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if cfg.flowsPerPkt < 1 {
		cfg.flowsPerPkt = 1
	}
	if cfg.flowsPerPkt > v5MaxFlows {
		fmt.Fprintf(os.Stderr, "note: clamping flows-per-packet %d to v5 max %d\n", cfg.flowsPerPkt, v5MaxFlows)
		cfg.flowsPerPkt = v5MaxFlows
	}
	if cfg.dnsPct+cfg.privatePct+cfg.zeroBytePct > 100 {
		return fmt.Errorf("dns+private+zero-byte percentages exceed 100")
	}
	if cfg.pps < 1 {
		return fmt.Errorf("pps must be >= 1")
	}
	if cfg.senders < 1 {
		cfg.senders = 1
	}
	if cfg.ringSize < 1 {
		cfg.ringSize = 1
	}

	ring := buildRing(cfg)
	pktBytes := int64(v5HeaderLen + cfg.flowsPerPkt*v5RecordLen)

	fmt.Printf("benchgen -> %s | pps=%d senders=%d flows/pkt=%d (%.1f flows/s) dur=%s mix[dns=%d%% priv=%d%% zero=%d%%]\n",
		cfg.target, cfg.pps, cfg.senders, cfg.flowsPerPkt,
		float64(cfg.pps)*float64(cfg.flowsPerPkt), cfg.duration,
		cfg.dnsPct, cfg.privatePct, cfg.zeroBytePct)

	var c counters
	stop := make(chan struct{})

	// Per-second reporter.
	var repWG sync.WaitGroup
	repWG.Add(1)
	go report(&c, pktBytes, stop, &repWG)

	// Senders.
	perSender := float64(cfg.pps) / float64(cfg.senders)
	var wg sync.WaitGroup
	start := time.Now()
	for s := 0; s < cfg.senders; s++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sender(cfg, ring, perSender, start, &c, id)
		}(s)
	}
	wg.Wait()
	close(stop)
	repWG.Wait()

	elapsed := time.Since(start).Seconds()
	pk, fl, by, er := c.packets.Load(), c.flows.Load(), c.bytes.Load(), c.errors.Load()
	fmt.Printf("\nSUMMARY: %.1fs | packets=%d (%.0f pps) | flows=%d (%.0f flows/s) | %.1f Mbps | send_errors=%d\n",
		elapsed, pk, float64(pk)/elapsed, fl, float64(fl)/elapsed,
		float64(by)*8/1e6/elapsed, er)
	return nil
}

// sender paces its share of the packet rate using a self-correcting accumulator
// and writes pre-built packets round-robin over the ring.
func sender(cfg config, ring [][]byte, perSec float64, start time.Time, c *counters, id int) {
	conn, err := net.Dial("udp", cfg.target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sender %d dial: %v\n", id, err)
		return
	}
	defer conn.Close()

	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
	idx := rng.Intn(len(ring))
	var sent int64

	for {
		elapsed := time.Since(start)
		if elapsed >= cfg.duration {
			return
		}
		target := int64(elapsed.Seconds() * perSec)
		for sent < target {
			pkt := ring[idx%len(ring)]
			idx++
			if _, err := conn.Write(pkt); err != nil {
				c.errors.Add(1)
			} else {
				c.packets.Add(1)
				c.flows.Add(int64(cfg.flowsPerPkt))
				c.bytes.Add(int64(len(pkt)))
			}
			sent++
		}
		time.Sleep(150 * time.Microsecond)
	}
}

func report(c *counters, pktBytes int64, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	_ = pktBytes
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastP, lastF, lastB, lastE int64
	sec := 0
	fmt.Printf("%4s  %12s  %12s  %9s  %11s\n", "sec", "packets/s", "flows/s", "mbps", "send_errs")
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			sec++
			p, f, b, e := c.packets.Load(), c.flows.Load(), c.bytes.Load(), c.errors.Load()
			dp, df, db, de := p-lastP, f-lastF, b-lastB, e-lastE
			lastP, lastF, lastB, lastE = p, f, b, e
			fmt.Printf("%4d  %12d  %12d  %9.1f  %11d\n", sec, dp, df, float64(db)*8/1e6, de)
		}
	}
}

// buildRing pre-builds a ring of distinct valid v5 packets whose flows follow
// the requested category mix, so the hot send loop does no per-packet encoding.
func buildRing(cfg config) [][]byte {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	now := uint32(time.Now().Unix())
	sysUptime := uint32(3_600_000) // 1h of uptime in ms

	ring := make([][]byte, cfg.ringSize)
	for i := range ring {
		pkt := make([]byte, v5HeaderLen+cfg.flowsPerPkt*v5RecordLen)
		binary.BigEndian.PutUint16(pkt[0:2], 5)
		binary.BigEndian.PutUint16(pkt[2:4], uint16(cfg.flowsPerPkt))
		binary.BigEndian.PutUint32(pkt[4:8], sysUptime)
		binary.BigEndian.PutUint32(pkt[8:12], now)
		binary.BigEndian.PutUint32(pkt[12:16], 0)         // unix_nsecs
		binary.BigEndian.PutUint32(pkt[16:20], uint32(i)) // flow_sequence

		for j := 0; j < cfg.flowsPerPkt; j++ {
			off := v5HeaderLen + j*v5RecordLen
			fillRecord(pkt[off:off+v5RecordLen], cfg, rng, sysUptime)
		}
		ring[i] = pkt
	}
	return ring
}

// fillRecord writes one 48-byte v5 record of a category chosen by the mix.
func fillRecord(r []byte, cfg config, rng *rand.Rand, sysUptime uint32) {
	var src, dst net.IP
	var srcPort, dstPort uint16
	octets := uint32(64 + rng.Intn(1<<16))
	pkts := uint32(1 + rng.Intn(64))

	switch category(cfg, rng) {
	case catDNS:
		src = privateIP(rng)
		dst = publicIP(rng)
		srcPort = ephemeralPort(rng)
		dstPort = 53
	case catPrivate:
		src = privateIP(rng)
		dst = privateIP(rng)
		srcPort = ephemeralPort(rng)
		dstPort = uint16(1 + rng.Intn(1024))
	case catZero:
		src = privateIP(rng)
		dst = publicIP(rng)
		srcPort = ephemeralPort(rng)
		dstPort = 443
		octets = 0
	default: // normal
		src = privateIP(rng)
		dst = publicIP(rng)
		srcPort = ephemeralPort(rng)
		dstPort = []uint16{80, 443, 8080, 22, 3478}[rng.Intn(5)]
	}

	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint32(r[16:20], pkts)
	binary.BigEndian.PutUint32(r[20:24], octets)
	binary.BigEndian.PutUint32(r[24:28], sysUptime-uint32(2000+rng.Intn(60000))) // first
	binary.BigEndian.PutUint32(r[28:32], sysUptime-uint32(rng.Intn(2000)))       // last
	binary.BigEndian.PutUint16(r[32:34], srcPort)
	binary.BigEndian.PutUint16(r[34:36], dstPort)
	r[37] = 0x10 // tcp flags (ACK)
	r[38] = 6    // TCP
	r[44] = 24   // src mask
	r[45] = 24   // dst mask
}

type cat int

const (
	catNormal cat = iota
	catDNS
	catPrivate
	catZero
)

func category(cfg config, rng *rand.Rand) cat {
	r := rng.Intn(100)
	if r < cfg.dnsPct {
		return catDNS
	}
	if r < cfg.dnsPct+cfg.privatePct {
		return catPrivate
	}
	if r < cfg.dnsPct+cfg.privatePct+cfg.zeroBytePct {
		return catZero
	}
	return catNormal
}

func privateIP(rng *rand.Rand) net.IP {
	return net.IPv4(10, byte(rng.Intn(256)), byte(rng.Intn(256)), byte(1+rng.Intn(254)))
}

func publicIP(rng *rand.Rand) net.IP {
	first := []byte{1, 3, 4, 8, 13, 23, 34, 52, 99, 104, 142, 151, 185, 203}[rng.Intn(14)]
	return net.IPv4(first, byte(rng.Intn(256)), byte(rng.Intn(256)), byte(1+rng.Intn(254)))
}

func ephemeralPort(rng *rand.Rand) uint16 {
	return uint16(1024 + rng.Intn(64000))
}
