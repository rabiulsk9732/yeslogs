package director

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/natflow/natflow-dataplane/internal/director/store"
)

// FlowReader runs read-only, ISP-scoped queries against the ClickHouse flow_logs
// table. The isp filter is always supplied by the caller (the handler derives it
// from the authenticated identity), so a tenant can never read another's flows.
type FlowReader struct {
	conn driver.Conn
	db   string
}

// NewFlowReader connects to ClickHouse for read queries.
func NewFlowReader(addr, db, user, pass string) (*FlowReader, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:         []string{addr},
		Auth:         clickhouse.Auth{Database: db, Username: user, Password: pass},
		DialTimeout:  5 * time.Second,
		MaxOpenConns: 8, // dashboard fans out ~7 concurrent aggregations per load
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ping clickhouse %s: %w", addr, err)
	}
	return &FlowReader{conn: conn, db: db}, nil
}

func (r *FlowReader) Close() error { return r.conn.Close() }

// scope builds the WHERE clause + args. ispID 0 means "all ISPs" (director only).
func scope(ispID uint32, days int) (string, []any) {
	where := fmt.Sprintf("event_date >= today() - %d", days)
	var args []any
	if ispID != 0 {
		where += " AND isp_id = ?"
		args = append(args, ispID)
	}
	return where, args
}

// Summary aggregates totals for the window.
type Summary struct {
	Rows    uint64
	Bytes   uint64
	Packets uint64
	Devices uint64
}

func (r *FlowReader) Summary(ctx context.Context, ispID uint32, days int) (Summary, error) {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT count(), sum(bytes), sum(packets), uniq(device_id)
		FROM %s.flow_logs WHERE %s`, r.db, where)
	var s Summary
	err := r.conn.QueryRow(ctx, q, args...).Scan(&s.Rows, &s.Bytes, &s.Packets, &s.Devices)
	return s, err
}

// Talker is a top source by bytes.
type Talker struct {
	SrcIP string
	Bytes uint64
	Flows uint64
}

func (r *FlowReader) TopTalkers(ctx context.Context, ispID uint32, days, limit int) ([]Talker, error) {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT src_ip, sum(bytes) AS b, count() AS f FROM %s.flow_logs
		WHERE %s GROUP BY src_ip ORDER BY b DESC LIMIT %d`, r.db, where, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Talker
	for rows.Next() {
		var ip net.IP
		var t Talker
		if err := rows.Scan(&ip, &t.Bytes, &t.Flows); err != nil {
			return nil, err
		}
		t.SrcIP = ip.String()
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeviceStat is per-device traffic.
type DeviceStat struct {
	ISPID    uint32
	DeviceID uint32
	Flows    uint64
	Bytes    uint64
}

func (r *FlowReader) PerDevice(ctx context.Context, ispID uint32, days int) ([]DeviceStat, error) {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT isp_id, device_id, count() AS f, sum(bytes) AS b FROM %s.flow_logs
		WHERE %s GROUP BY isp_id, device_id ORDER BY b DESC LIMIT 200`, r.db, where)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceStat
	for rows.Next() {
		var d DeviceStat
		if err := rows.Scan(&d.ISPID, &d.DeviceID, &d.Flows, &d.Bytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// LastSeenByExporter returns the most recent flow_start per (isp_id, exporter_ip)
// within the lookback window, keyed by "ispID|dotted-ip". Used by the device
// liveness monitor and the Devices status badges. The map values are correct
// instants regardless of display timezone (safe to compare with time.Now()).
func (r *FlowReader) LastSeenByExporter(ctx context.Context, days int, devs []store.Device) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(devs) == 0 {
		return out, nil
	}

	var queries []string
	for _, d := range devs {
		if !d.Enabled {
			continue
		}
		// A fast index lookup per device, unioned into a single round-trip query.
		// ifNull handles devices with no recent flows (subquery returns no rows).
		queries = append(queries, fmt.Sprintf(
			`SELECT %d AS isp, '%s' AS ip, ifNull((SELECT flow_start FROM %s.flow_logs WHERE isp_id=%d AND device_id=%d AND event_date >= today() - %d ORDER BY flow_start DESC LIMIT 1), toDateTime('1970-01-01')) AS ts`,
			d.ISPID, d.ExporterIP, r.db, d.ISPID, d.ID, days))
	}
	if len(queries) == 0 {
		return out, nil
	}
	q := strings.Join(queries, " UNION ALL ")

	rows, err := r.conn.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var isp uint32
		var ip string
		var ts time.Time
		if err := rows.Scan(&isp, &ip, &ts); err != nil {
			return nil, err
		}
		if ts.Unix() > 0 {
			out[fmt.Sprintf("%d|%s", isp, ip)] = ts
		}
	}
	return out, rows.Err()
}

// RecentFlow is one recent flow row for display.
type RecentFlow struct {
	ISPID     uint32
	DeviceID  uint32
	SrcIP     string
	SrcPort   uint16
	DstIP     string
	DstPort   uint16
	Protocol  uint8
	Bytes     uint64
	FlowType  string
	FlowStart time.Time
}

func (r *FlowReader) Recent(ctx context.Context, ispID uint32, days, limit int) ([]RecentFlow, error) {
	where, args := scope(ispID, days)
	q := fmt.Sprintf(`SELECT isp_id, device_id, src_ip, src_port, dst_ip, dst_port, protocol, bytes, flow_type, flow_start
		FROM %s.flow_logs WHERE %s ORDER BY flow_start DESC LIMIT %d`, r.db, where, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecentFlow
	for rows.Next() {
		var f RecentFlow
		var src, dst net.IP
		if err := rows.Scan(&f.ISPID, &f.DeviceID, &src, &f.SrcPort, &dst, &f.DstPort, &f.Protocol, &f.Bytes, &f.FlowType, &f.FlowStart); err != nil {
			return nil, err
		}
		f.SrcIP, f.DstIP = src.String(), dst.String()
		out = append(out, f)
	}
	return out, rows.Err()
}
