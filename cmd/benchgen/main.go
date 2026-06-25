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
	proto       string
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
	flag.StringVar(&cfg.proto, "proto", "netflow5", "protocol to generate: netflow5 | netflow9")
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
	if cfg.proto != "netflow5" && cfg.proto != "netflow9" && cfg.proto != "ipfix" {
		return fmt.Errorf("proto must be netflow5, netflow9 or ipfix, got %q", cfg.proto)
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

	ring, tmpl := buildRing(cfg)

	fmt.Printf("benchgen [%s] -> %s | pps=%d senders=%d flows/pkt=%d (%.1f flows/s) dur=%s mix[dns=%d%% priv=%d%% zero=%d%%]\n",
		cfg.proto, cfg.target, cfg.pps, cfg.senders, cfg.flowsPerPkt,
		float64(cfg.pps)*float64(cfg.flowsPerPkt), cfg.duration,
		cfg.dnsPct, cfg.privatePct, cfg.zeroBytePct)

	var c counters
	stop := make(chan struct{})

	// Per-second reporter.
	var repWG sync.WaitGroup
	repWG.Add(1)
	go report(&c, stop, &repWG)

	// Senders.
	perSender := float64(cfg.pps) / float64(cfg.senders)
	var wg sync.WaitGroup
	start := time.Now()
	for s := 0; s < cfg.senders; s++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sender(cfg, ring, tmpl, perSender, start, &c, id)
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

// templateRefreshEvery re-sends the v9 template periodically so a fresh
// collector (or one that expired the template) can resume decoding.
const templateRefreshEvery = 2000

// sender paces its share of the packet rate using a self-correcting accumulator
// and writes pre-built packets round-robin over the ring. For NetFlow v9 it
// injects the template packet first and every templateRefreshEvery packets.
func sender(cfg config, ring [][]byte, tmpl []byte, perSec float64, start time.Time, c *counters, id int) {
	conn, err := net.Dial("udp", cfg.target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sender %d dial: %v\n", id, err)
		return
	}
	defer conn.Close()

	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
	idx := rng.Intn(len(ring))
	var sent, n int64

	for {
		elapsed := time.Since(start)
		if elapsed >= cfg.duration {
			return
		}
		target := int64(elapsed.Seconds() * perSec)
		for sent < target {
			var pkt []byte
			flows := int64(0)
			if tmpl != nil && n%templateRefreshEvery == 0 {
				pkt = tmpl // v9 template (carries no flows)
			} else {
				pkt = ring[idx%len(ring)]
				idx++
				flows = int64(cfg.flowsPerPkt)
			}
			n++
			if _, err := conn.Write(pkt); err != nil {
				c.errors.Add(1)
			} else {
				c.packets.Add(1)
				c.flows.Add(flows)
				c.bytes.Add(int64(len(pkt)))
			}
			sent++
		}
		time.Sleep(150 * time.Microsecond)
	}
}

func report(c *counters, stop <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
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

// buildRing pre-builds a ring of distinct valid packets whose flows follow the
// requested category mix, so the hot send loop does no per-packet encoding. For
// NetFlow v9 it also returns the template packet to inject periodically.
func buildRing(cfg config) (ring [][]byte, tmpl []byte) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	now := uint32(time.Now().Unix())
	sysUptime := uint32(3_600_000) // 1h of uptime in ms

	ring = make([][]byte, cfg.ringSize)
	switch cfg.proto {
	case "netflow9":
		tmpl = v9TemplatePacket(now, sysUptime)
		for i := range ring {
			ring[i] = v9DataPacket(cfg, rng, now, sysUptime)
		}
		return ring, tmpl
	case "ipfix":
		tmpl = ipfixTemplatePacket(now)
		for i := range ring {
			ring[i] = ipfixDataPacket(cfg, rng, now)
		}
		return ring, tmpl
	}
	for i := range ring {
		ring[i] = v5Packet(cfg, rng, now, sysUptime, i)
	}
	return ring, nil
}

func v5Packet(cfg config, rng *rand.Rand, now, sysUptime uint32, seq int) []byte {
	pkt := make([]byte, v5HeaderLen+cfg.flowsPerPkt*v5RecordLen)
	binary.BigEndian.PutUint16(pkt[0:2], 5)
	binary.BigEndian.PutUint16(pkt[2:4], uint16(cfg.flowsPerPkt))
	binary.BigEndian.PutUint32(pkt[4:8], sysUptime)
	binary.BigEndian.PutUint32(pkt[8:12], now)
	binary.BigEndian.PutUint32(pkt[12:16], 0)           // unix_nsecs
	binary.BigEndian.PutUint32(pkt[16:20], uint32(seq)) // flow_sequence
	for j := 0; j < cfg.flowsPerPkt; j++ {
		off := v5HeaderLen + j*v5RecordLen
		fillRecord(pkt[off:off+v5RecordLen], cfg, rng, sysUptime)
	}
	return pkt
}

// --- NetFlow v9 generation -------------------------------------------------

const (
	v9TemplateID = 256
	v9SourceID   = 1
	v9RecordLen  = 29 // src4 dst4 sport2 dport2 proto1 bytes4 pkts4 first4 last4
)

// v9 field (type,length) pairs matching the record layout below.
var v9TemplateFields = [][2]uint16{
	{8, 4}, {12, 4}, {7, 2}, {11, 2}, {4, 1}, {1, 4}, {2, 4}, {22, 4}, {21, 4},
}

func v9Header(count uint16, now, sysUptime uint32) []byte {
	h := make([]byte, 20)
	binary.BigEndian.PutUint16(h[0:2], 9)
	binary.BigEndian.PutUint16(h[2:4], count)
	binary.BigEndian.PutUint32(h[4:8], sysUptime)
	binary.BigEndian.PutUint32(h[8:12], now)
	binary.BigEndian.PutUint32(h[12:16], 0)          // package_sequence
	binary.BigEndian.PutUint32(h[16:20], v9SourceID) // source_id
	return h
}

func v9TemplatePacket(now, sysUptime uint32) []byte {
	body := make([]byte, 4+len(v9TemplateFields)*4)
	binary.BigEndian.PutUint16(body[0:2], v9TemplateID)
	binary.BigEndian.PutUint16(body[2:4], uint16(len(v9TemplateFields)))
	for i, f := range v9TemplateFields {
		o := 4 + i*4
		binary.BigEndian.PutUint16(body[o:o+2], f[0])
		binary.BigEndian.PutUint16(body[o+2:o+4], f[1])
	}
	fs := make([]byte, 4+len(body))
	binary.BigEndian.PutUint16(fs[0:2], 0) // template flowset id
	binary.BigEndian.PutUint16(fs[2:4], uint16(len(fs)))
	copy(fs[4:], body)
	return append(v9Header(1, now, sysUptime), fs...)
}

func v9DataPacket(cfg config, rng *rand.Rand, now, sysUptime uint32) []byte {
	dataLen := cfg.flowsPerPkt * v9RecordLen
	fs := make([]byte, 4+dataLen)
	binary.BigEndian.PutUint16(fs[0:2], v9TemplateID) // data flowset id == template id
	binary.BigEndian.PutUint16(fs[2:4], uint16(len(fs)))
	for j := 0; j < cfg.flowsPerPkt; j++ {
		off := 4 + j*v9RecordLen
		fillV9Record(fs[off:off+v9RecordLen], cfg, rng, sysUptime)
	}
	return append(v9Header(uint16(cfg.flowsPerPkt), now, sysUptime), fs...)
}

func fillV9Record(r []byte, cfg config, rng *rand.Rand, sysUptime uint32) {
	var src, dst net.IP
	var sp, dp uint16
	octets := uint32(64 + rng.Intn(1<<16))
	pkts := uint32(1 + rng.Intn(64))
	proto := uint8(6)

	switch category(cfg, rng) {
	case catDNS:
		src, dst, sp, dp, proto = privateIP(rng), publicIP(rng), ephemeralPort(rng), 53, 17
	case catPrivate:
		src, dst, sp, dp = privateIP(rng), privateIP(rng), ephemeralPort(rng), uint16(1+rng.Intn(1024))
	case catZero:
		src, dst, sp, dp, octets = privateIP(rng), publicIP(rng), ephemeralPort(rng), 443, 0
	default:
		src, dst, sp, dp = privateIP(rng), publicIP(rng), ephemeralPort(rng), []uint16{80, 443, 8080, 22}[rng.Intn(4)]
	}

	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint16(r[8:10], sp)
	binary.BigEndian.PutUint16(r[10:12], dp)
	r[12] = proto
	binary.BigEndian.PutUint32(r[13:17], octets)
	binary.BigEndian.PutUint32(r[17:21], pkts)
	binary.BigEndian.PutUint32(r[21:25], sysUptime-uint32(2000+rng.Intn(60000))) // first
	binary.BigEndian.PutUint32(r[25:29], sysUptime-uint32(rng.Intn(2000)))       // last
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

// --- IPFIX generation ------------------------------------------------------

const (
	ipfixTemplateID = 256
	ipfixRecordLen  = 37 // src4 dst4 sport2 dport2 proto1 octets4 pkts4 startMs8 endMs8
)

// IPFIX (type,length) specifiers matching the record layout below.
var ipfixTemplateFields = [][2]uint16{
	{8, 4}, {12, 4}, {7, 2}, {11, 2}, {4, 1}, {1, 4}, {2, 4}, {152, 8}, {153, 8},
}

func ipfixHeader(msgLen int, now uint32) []byte {
	h := make([]byte, 16)
	binary.BigEndian.PutUint16(h[0:2], 10)
	binary.BigEndian.PutUint16(h[2:4], uint16(msgLen))
	binary.BigEndian.PutUint32(h[4:8], now) // export time (seconds)
	binary.BigEndian.PutUint32(h[8:12], 0)  // sequence number
	binary.BigEndian.PutUint32(h[12:16], 1) // observation domain id
	return h
}

func ipfixTemplatePacket(now uint32) []byte {
	rec := make([]byte, 4+len(ipfixTemplateFields)*4)
	binary.BigEndian.PutUint16(rec[0:2], ipfixTemplateID)
	binary.BigEndian.PutUint16(rec[2:4], uint16(len(ipfixTemplateFields)))
	for i, f := range ipfixTemplateFields {
		o := 4 + i*4
		binary.BigEndian.PutUint16(rec[o:o+2], f[0])
		binary.BigEndian.PutUint16(rec[o+2:o+4], f[1])
	}
	set := make([]byte, 4+len(rec))
	binary.BigEndian.PutUint16(set[0:2], 2) // Template Set id
	binary.BigEndian.PutUint16(set[2:4], uint16(len(set)))
	copy(set[4:], rec)
	return append(ipfixHeader(16+len(set), now), set...)
}

func ipfixDataPacket(cfg config, rng *rand.Rand, now uint32) []byte {
	dataLen := cfg.flowsPerPkt * ipfixRecordLen
	set := make([]byte, 4+dataLen)
	binary.BigEndian.PutUint16(set[0:2], ipfixTemplateID) // Data Set id == template id
	binary.BigEndian.PutUint16(set[2:4], uint16(len(set)))
	for j := 0; j < cfg.flowsPerPkt; j++ {
		off := 4 + j*ipfixRecordLen
		fillIPFIXRecord(set[off:off+ipfixRecordLen], cfg, rng, now)
	}
	return append(ipfixHeader(16+len(set), now), set...)
}

func fillIPFIXRecord(r []byte, cfg config, rng *rand.Rand, now uint32) {
	var src, dst net.IP
	var sp, dp uint16
	octets := uint32(64 + rng.Intn(1<<16))
	pkts := uint32(1 + rng.Intn(64))
	proto := uint8(6)

	switch category(cfg, rng) {
	case catDNS:
		src, dst, sp, dp, proto = privateIP(rng), publicIP(rng), ephemeralPort(rng), 53, 17
	case catPrivate:
		src, dst, sp, dp = privateIP(rng), privateIP(rng), ephemeralPort(rng), uint16(1+rng.Intn(1024))
	case catZero:
		src, dst, sp, dp, octets = privateIP(rng), publicIP(rng), ephemeralPort(rng), 443, 0
	default:
		src, dst, sp, dp = privateIP(rng), publicIP(rng), ephemeralPort(rng), []uint16{80, 443, 8080, 22}[rng.Intn(4)]
	}

	nowMs := uint64(now) * 1000
	copy(r[0:4], src.To4())
	copy(r[4:8], dst.To4())
	binary.BigEndian.PutUint16(r[8:10], sp)
	binary.BigEndian.PutUint16(r[10:12], dp)
	r[12] = proto
	binary.BigEndian.PutUint32(r[13:17], octets)
	binary.BigEndian.PutUint32(r[17:21], pkts)
	binary.BigEndian.PutUint64(r[21:29], nowMs-uint64(2000+rng.Intn(60000))) // flowStartMs
	binary.BigEndian.PutUint64(r[29:37], nowMs-uint64(rng.Intn(2000)))       // flowEndMs
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
