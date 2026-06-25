// Command pcapreplay replays the UDP payloads of a pcap to a target host:port,
// preserving capture timing (scaled by --speed). Useful for replaying a
// (sanitized) real-device capture into a collector for validation.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pcapreplay:", err)
		os.Exit(1)
	}
}

func run() error {
	pcapPath := flag.String("pcap", "", "path to the .pcap file")
	target := flag.String("target", "127.0.0.1:2055", "destination host:port")
	speedStr := flag.String("speed", "1x", "replay speed, e.g. 1x, 10x (or 'max')")
	loop := flag.Bool("loop", false, "replay repeatedly until interrupted")
	protoStr := flag.String("proto", "auto", "auto|netflow5|netflow9|ipfix")
	flag.Parse()

	if *pcapPath == "" {
		return fmt.Errorf("--pcap is required")
	}
	pf, err := replay.ParseProto(*protoStr)
	if err != nil {
		return err
	}
	speed, err := parseSpeed(*speedStr)
	if err != nil {
		return err
	}

	conn, err := net.Dial("udp", *target)
	if err != nil {
		return fmt.Errorf("dial %s: %w", *target, err)
	}
	defer conn.Close()
	send := func(b []byte) error { _, e := conn.Write(b); return e }

	fmt.Printf("replaying %s -> %s (speed=%s proto=%s loop=%v)\n", *pcapPath, *target, *speedStr, *protoStr, *loop)
	var total replay.Stats
	pass := 0
	for {
		f, err := os.Open(*pcapPath)
		if err != nil {
			return err
		}
		st, err := replay.Stream(f, pf, speed, send)
		f.Close()
		if err != nil {
			return err
		}
		total.Packets += st.Packets
		total.Sent += st.Sent
		total.Bytes += st.Bytes
		pass++
		fmt.Printf("pass %d: sent %d payloads (%d bytes) of %d udp packets\n", pass, st.Sent, st.Bytes, st.Packets)
		if !*loop {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("done: %d passes, %d payloads sent\n", pass, total.Sent)
	return nil
}

// parseSpeed parses "1x", "10x", "2.5", or "max".
func parseSpeed(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "max" || s == "0" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "x")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid speed %q", s)
	}
	return v, nil
}
