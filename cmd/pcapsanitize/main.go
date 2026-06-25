// Command pcapsanitize reads a pcap and writes a copy with all IPv4 addresses
// deterministically rewritten — in the IP/UDP headers AND inside NetFlow/IPFIX
// flow records — so a real-device capture can be shared as a fixture without
// exposing customer data. Original addresses are never printed.
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/natflow/natflow-dataplane/internal/pcapio"
	"github.com/natflow/natflow-dataplane/internal/sanitize"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pcapsanitize:", err)
		os.Exit(1)
	}
}

func run() error {
	in := flag.String("in", "", "input pcap (may contain real addresses)")
	out := flag.String("out", "", "output sanitized pcap")
	keyHex := flag.String("key", "", "hex sanitization key (default: random per run)")
	flag.Parse()

	if *in == "" || *out == "" {
		return fmt.Errorf("--in and --out are required")
	}
	var key []byte
	if *keyHex != "" {
		k, err := hex.DecodeString(*keyHex)
		if err != nil {
			return fmt.Errorf("invalid --key hex: %w", err)
		}
		key = k
	}

	fin, err := os.Open(*in)
	if err != nil {
		return err
	}
	defer fin.Close()
	rd, err := pcapio.NewReader(fin)
	if err != nil {
		return err
	}

	fout, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer fout.Close()
	wr, err := pcapio.NewWriter(fout, rd.LinkType())
	if err != nil {
		return err
	}

	s := sanitize.New(key)
	for {
		p, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if s.Frame(rd.LinkType(), p.Data) {
			if err := wr.Write(p); err != nil {
				return err
			}
		}
	}

	st := s.Stats
	// Deliberately does NOT print any original/sanitized address.
	fmt.Printf("sanitized %s -> %s: packets=%d kept=%d dropped=%d ips_rewritten=%d\n",
		*in, *out, st.Packets, st.Kept, st.Dropped, st.IPsRewritten)
	if st.Dropped > 0 {
		fmt.Printf("note: %d packet(s) dropped (unknown template / unparseable; not safely sanitizable)\n", st.Dropped)
	}
	return nil
}
