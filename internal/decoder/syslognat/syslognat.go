// Package syslognat decodes common firewall NAT syslog messages after their
// RFC 3164/5424 envelope. It intentionally accepts vendor key aliases because
// firmware versions rename fields while retaining the same meaning.
package syslognat

import (
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/natflow/natflow-dataplane/internal/decoder"
)

type Decoder struct{}

func New() *Decoder           { return &Decoder{} }
func (*Decoder) Kind() string { return "syslog" }

var (
	kvRE           = regexp.MustCompile(`([A-Za-z][A-Za-z0-9_-]*)=(?:"([^"]*)"|'([^']*)'|([^\s]+))`)
	asaTranslation = regexp.MustCompile(`(?i)\b(Built|Teardown)\s+(?:dynamic\s+)?(?:TCP|UDP|ICMP)?\s*translation\s+from\s+\S+:([0-9a-f:.]+)/([0-9]+)\s+to\s+\S+:([0-9a-f:.]+)/([0-9]+)`)
)

func (*Decoder) Decode(dst []decoder.Flow, payload []byte, exporter net.IP) ([]decoder.Flow, error) {
	msg := strings.TrimSpace(string(payload))
	if msg == "" {
		return dst, errors.New("empty syslog message")
	}
	fields := map[string]string{}
	for _, m := range kvRE.FindAllStringSubmatch(msg, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		if v == "" {
			v = m[4]
		}
		fields[strings.ToLower(m[1])] = strings.Trim(v, `"`)
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := fields[k]; v != "" {
				return v
			}
		}
		return ""
	}
	var f decoder.Flow
	f.ExporterIP = append(net.IP(nil), exporter...)
	f.SrcIP = net.ParseIP(get("srcip", "src_ip", "sourceip", "source_ip"))
	f.DstIP = net.ParseIP(get("dstip", "dst_ip", "destinationip", "destination_ip"))
	f.NatPublicIP = net.ParseIP(get("transip", "translated_ip", "nat_src_ip", "src_translated_ip", "xlatesrc"))
	f.NatDestIP = net.ParseIP(get("trandisp", "nat_dst_ip", "dst_translated_ip", "xlatedst"))
	f.SrcPort = parsePort(get("srcport", "src_port", "sourceport"))
	f.DstPort = parsePort(get("dstport", "dst_port", "destinationport"))
	f.NatPublicPort = parsePort(get("transport", "translated_port", "nat_src_port", "src_translated_port"))
	f.NatDestPort = parsePort(get("tranport", "nat_dst_port", "dst_translated_port"))
	f.Protocol = parseProtocol(get("proto", "protocol"))
	f.Bytes = parseUint(get("sentbyte", "rcvdbyte", "bytes"))
	f.Packets = parseUint(get("sentpkt", "rcvdpkt", "packets"))
	action := strings.ToLower(get("action", "eventtype", "log_subtype"))
	if strings.Contains(action, "delete") || strings.Contains(action, "teardown") || strings.Contains(action, "close") {
		f.NatEvent = 2
	} else if f.NatPublicIP != nil || f.NatDestIP != nil {
		f.NatEvent = 1
	}
	if f.SrcIP == nil {
		if m := asaTranslation.FindStringSubmatch(msg); m != nil {
			f.SrcIP = net.ParseIP(m[2])
			f.SrcPort = parsePort(m[3])
			f.NatPublicIP = net.ParseIP(m[4])
			f.NatPublicPort = parsePort(m[5])
			if strings.EqualFold(m[1], "Teardown") {
				f.NatEvent = 2
			} else {
				f.NatEvent = 1
			}
			if strings.Contains(strings.ToUpper(msg), "TCP") {
				f.Protocol = 6
			} else if strings.Contains(strings.ToUpper(msg), "UDP") {
				f.Protocol = 17
			}
		}
	}
	if f.SrcIP == nil || (f.NatPublicIP == nil && f.NatDestIP == nil) {
		return dst, errors.New("syslog message contains no supported NAT mapping")
	}
	now := time.Now().UTC()
	f.FlowStart = now
	f.FlowEnd = now
	return append(dst, f), nil
}

func parseUint(s string) uint64 { n, _ := strconv.ParseUint(s, 10, 64); return n }
func parsePort(s string) uint16 { n, _ := strconv.ParseUint(s, 10, 16); return uint16(n) }
func parseProtocol(s string) uint8 {
	switch strings.ToLower(s) {
	case "tcp":
		return 6
	case "udp":
		return 17
	case "icmp":
		return 1
	}
	n, _ := strconv.ParseUint(s, 10, 8)
	return uint8(n)
}
