// Package replay reads a pcap and re-sends its UDP payloads, preserving
// inter-packet timing (scaled by speed). It is the reusable core behind
// cmd/pcapreplay.
package replay

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/natflow/natflow-dataplane/internal/pcapio"
)

// Proto filters which flow protocol to replay.
type Proto int

const (
	Any Proto = iota
	NetFlow5
	NetFlow9
	IPFIX
)

// ParseProto parses a --proto value.
func ParseProto(s string) (Proto, error) {
	switch s {
	case "", "auto":
		return Any, nil
	case "netflow5":
		return NetFlow5, nil
	case "netflow9":
		return NetFlow9, nil
	case "ipfix":
		return IPFIX, nil
	default:
		return Any, fmt.Errorf("invalid proto %q (auto|netflow5|netflow9|ipfix)", s)
	}
}

func matches(pf Proto, payload []byte) bool {
	if pf == Any {
		return true
	}
	if len(payload) < 2 {
		return false
	}
	v := binary.BigEndian.Uint16(payload[0:2])
	switch pf {
	case NetFlow5:
		return v == 5
	case NetFlow9:
		return v == 9
	case IPFIX:
		return v == 10
	default:
		return true
	}
}

// Stats summarizes a replay pass.
type Stats struct {
	Packets int // UDP/IPv4 packets seen
	Sent    int // payloads sent (matching the filter)
	Bytes   int // payload bytes sent
}

// Stream reads a pcap from r and sends each matching IPv4/UDP payload via send,
// sleeping between packets to reproduce capture timing divided by speed
// (speed <= 0 sends as fast as possible).
func Stream(r io.Reader, pf Proto, speed float64, send func([]byte) error) (Stats, error) {
	rd, err := pcapio.NewReader(r)
	if err != nil {
		return Stats{}, err
	}
	lt := rd.LinkType()
	var st Stats
	var prev time.Time
	have := false
	for {
		p, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, err
		}
		info, ok := pcapio.ParseUDP(lt, p.Data)
		if !ok {
			continue
		}
		st.Packets++
		payload := p.Data[info.PayloadOff : info.PayloadOff+info.PayloadLen]
		if !matches(pf, payload) {
			continue
		}
		if have && speed > 0 {
			if d := p.Time.Sub(prev); d > 0 {
				time.Sleep(time.Duration(float64(d) / speed))
			}
		}
		prev, have = p.Time, true
		if err := send(payload); err != nil {
			return st, err
		}
		st.Sent++
		st.Bytes += len(payload)
	}
	return st, nil
}
