package director

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Pre-aggregated rollups keep the dashboard O(1): instead of scanning raw
// flow_logs (hundreds of millions to billions of rows) on every load, tiny
// per-(isp,day,hour,device) summaries are maintained and the dashboard reads a
// few hundred rows → sub-100ms regardless of raw size.
//
// These are maintained by a PERIODIC BATCH job (RollupTick, every ~2 min), NOT a
// per-insert materialized view. A per-block MV creates one rollup part per source
// insert block, so at high ingest (100s of exporters) part-creation outruns
// merges → "too many parts" insert rejection + broken parts on unclean restart.
// The batch job instead writes ~2 parts per run regardless of ingest rate, so it
// scales to any number of exporters. Freshness is bounded by the tick interval,
// which the dashboard's serve-stale cache already tolerates.
//
//   flow_rollup        — hourly stats per device (widgets, chart, proto, devices)
//   flow_rollup_subs   — daily bytes per subscriber (top-talkers)
//   flow_rollup_state  — watermark (max created_at rolled up so far)

func rollupDDL(db string) []string {
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.flow_rollup
(
    isp_id UInt32, event_date Date, hour UInt8, device_id UInt32,
    flows SimpleAggregateFunction(sum, UInt64),
    bytes SimpleAggregateFunction(sum, UInt64),
    packets SimpleAggregateFunction(sum, UInt64),
    tcp SimpleAggregateFunction(sum, UInt64),
    udp SimpleAggregateFunction(sum, UInt64),
    icmp SimpleAggregateFunction(sum, UInt64),
    other SimpleAggregateFunction(sum, UInt64),
    dur_sum SimpleAggregateFunction(sum, UInt64),
    subs AggregateFunction(uniq, IPv4),
    nat_ips AggregateFunction(uniq, IPv4)
) ENGINE = AggregatingMergeTree PARTITION BY event_date ORDER BY (isp_id, event_date, hour, device_id)`, db),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.flow_rollup_subs
(
    isp_id UInt32, event_date Date, src_ip IPv4,
    bytes SimpleAggregateFunction(sum, UInt64)
) ENGINE = SummingMergeTree PARTITION BY event_date ORDER BY (isp_id, event_date, src_ip)`, db),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.flow_rollup_state
(ts DateTime, at DateTime DEFAULT now()) ENGINE = MergeTree ORDER BY ts TTL at + INTERVAL 2 DAY`, db),
	}
}

// rollupInsertHourly / rollupInsertSubs build the aggregate INSERTs for a
// created_at window (exclusive lower, inclusive upper). Part-level min/max on
// created_at prunes to only the freshly-inserted parts, so each run scans just
// the last interval's data — cost is independent of total table size.
func (r *FlowReader) rollupInsertHourly(ctx context.Context, where string, args ...any) error {
	return r.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.flow_rollup
SELECT isp_id, event_date, toHour(flow_start) AS hour, device_id,
    sum(1), sum(bytes), sum(packets),
    countIf(protocol = 6), countIf(protocol = 17), countIf(protocol = 1), countIf(protocol NOT IN (6, 17, 1)),
    sum(toUInt64(flow_end - flow_start)), uniqState(src_ip), uniqState(nat_public_ip)
FROM %s.flow_logs WHERE %s GROUP BY isp_id, event_date, hour, device_id`, r.db, r.db, where), args...)
}

func (r *FlowReader) rollupInsertSubs(ctx context.Context, where string, args ...any) error {
	return r.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.flow_rollup_subs
SELECT isp_id, event_date, src_ip, sum(bytes) FROM %s.flow_logs
WHERE %s GROUP BY isp_id, event_date, src_ip`, r.db, r.db, where), args...)
}

// EnsureRollups creates the rollup tables (idempotent), migrates OFF the old
// per-insert materialized views if present, and seeds the watermark once.
func (r *FlowReader) EnsureRollups(ctx context.Context) error {
	for _, s := range rollupDDL(r.db) {
		if err := r.conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("rollup ddl: %w", err)
		}
	}
	// Migrate off the per-block MVs (part-explosion at scale) — the batch job owns
	// the rollup now. Dropping a MV does not touch its target table's data.
	for _, mv := range []string{"flow_rollup_mv", "flow_rollup_subs_mv"} {
		_ = r.conn.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`, r.db, mv))
	}
	// Watermark already seeded? Then we're initialized; RollupTick takes over.
	var seeded uint64
	if err := r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT count() FROM %s.flow_rollup_state`, r.db)).Scan(&seeded); err != nil {
		return fmt.Errorf("rollup state count: %w", err)
	}
	if seeded > 0 {
		return nil
	}
	cutoff := time.Now()
	// Fresh table → backfill everything up to the cutoff. If the rollup already has
	// data (migrated from the MV), skip the backfill and just start the watermark
	// at the cutoff (the batch job continues from here).
	var haveRollup uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT count() FROM %s.flow_rollup`, r.db)).Scan(&haveRollup)
	if haveRollup == 0 {
		if err := r.rollupInsertHourly(ctx, "created_at <= ?", cutoff); err != nil {
			return fmt.Errorf("rollup backfill: %w", err)
		}
		if err := r.rollupInsertSubs(ctx, "created_at <= ?", cutoff); err != nil {
			return fmt.Errorf("subs backfill: %w", err)
		}
	}
	if err := r.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.flow_rollup_state (ts) VALUES (?)`, r.db), cutoff); err != nil {
		return fmt.Errorf("seed watermark: %w", err)
	}
	return nil
}

// RollupTick rolls up flows ingested since the last watermark into the summary
// tables (~2 parts written per run, independent of ingest rate). Call periodically.
func (r *FlowReader) RollupTick(ctx context.Context) error {
	var wm time.Time
	if err := r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT max(ts) FROM %s.flow_rollup_state`, r.db)).Scan(&wm); err != nil {
		return fmt.Errorf("read watermark: %w", err)
	}
	cutoff := time.Now().Add(-10 * time.Second) // small lag so in-flight inserts settle
	if wm.IsZero() || !cutoff.After(wm) {
		return nil // not seeded yet, or nothing new
	}
	if err := r.rollupInsertHourly(ctx, "created_at > ? AND created_at <= ?", wm, cutoff); err != nil {
		return fmt.Errorf("rollup tick hourly: %w", err)
	}
	if err := r.rollupInsertSubs(ctx, "created_at > ? AND created_at <= ?", wm, cutoff); err != nil {
		return fmt.Errorf("rollup tick subs: %w", err)
	}
	return r.conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.flow_rollup_state (ts) VALUES (?)`, r.db), cutoff)
}

// hasRollup reports whether the rollup has any rows (used to fall back to raw
// aggregation until the rollup is populated).
func (r *FlowReader) hasRollup(ctx context.Context) bool {
	var n uint64
	if err := r.conn.QueryRow(ctx, fmt.Sprintf(`SELECT count() FROM %s.flow_rollup`, r.db)).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// ConsoleData serves the dashboard from the pre-aggregated rollup when it exists
// (sub-100ms at any raw-table size); it falls back to raw aggregation only until
// the rollup is populated.
func (r *FlowReader) ConsoleData(ctx context.Context, ispID uint32, days int) consoleData {
	if r.hasRollup(ctx) {
		return r.consoleDataRollup(ctx, ispID, days)
	}
	return r.consoleDataRaw(ctx, ispID, days)
}

func (r *FlowReader) consoleDataRollup(ctx context.Context, ispID uint32, days int) consoleData {
	var d consoleData
	where, args := scope(ispID, days)

	var flows, subs, devs, totBytes, natIPs, avgDur, tcp, udp, icmp, other uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT sum(flows), uniqMerge(subs), uniqExact(device_id), sum(bytes), uniqMerge(nat_ips),
		        toUInt64(sum(dur_sum)/greatest(sum(flows),1)), sum(tcp), sum(udp), sum(icmp), sum(other)
		 FROM %s.flow_rollup WHERE %s`, r.db, where), args...).
		Scan(&flows, &subs, &devs, &totBytes, &natIPs, &avgDur, &tcp, &udp, &icmp, &other)
	var today uint64
	_ = r.conn.QueryRow(ctx, fmt.Sprintf(
		`SELECT sum(flows) FROM %s.flow_rollup WHERE event_date = today()%s`, r.db, ispClause(ispID)), ispArgs(ispID)...).Scan(&today)

	d.Empty = flows == 0
	d.Widgets = []widget{
		{Value: group(flows), Label: "NAT Flows (window)", Icon: "fa-diagram-project", Color: "#0077b6"},
		{Value: group(today), Label: "Translations Today", Icon: "fa-right-left", Color: "#00a3c4"},
		{Value: group(subs), Label: "Subscribers Seen", Icon: "fa-users", Color: "#2a9d8f"},
		{Value: humanBytes2(totBytes), Label: "Logged Volume", Icon: "fa-shield-halved", Color: "#e76f51"},
	}
	poolPct := pctOf(natIPs, 256)
	d.InfoBoxes = []infoBox{
		{Label: "CGNAT Public IPs Seen", Value: group(natIPs), Pct: poolPct, Note: fmt.Sprintf("%s of /24 pool", poolPct), Icon: "fa-server", Color: "#0077b6"},
		{Label: "Active Devices", Value: group(devs), Pct: pctOf(devs, 50), Note: "exporters reporting", Icon: "fa-plug", Color: "#2a9d8f"},
		{Label: "Avg Session Duration", Value: dur(avgDur), Pct: "48%", Note: "flow_end − flow_start", Icon: "fa-clock", Color: "#e76f51"},
	}
	d.ProtoMix = protoMixFrom(tcp, udp, icmp, other)
	d.Hourly = r.rollupHourly(ctx, ispID)
	d.Region = r.rollupRegion(ctx, ispID, days)
	d.TopSubs = r.rollupTopSubs(ctx, ispID, days)
	d.Records = r.records(ctx, ispID, days, 50) // recent granules on raw — already cheap
	return d
}

func (r *FlowReader) rollupHourly(ctx context.Context, ispID uint32) []uint64 {
	out := make([]uint64, 24)
	rs, err := r.conn.Query(ctx, fmt.Sprintf(
		`SELECT hour, sum(flows) FROM %s.flow_rollup WHERE event_date = today()%s GROUP BY hour`, r.db, ispClause(ispID)), ispArgs(ispID)...)
	if err != nil {
		return out
	}
	defer rs.Close()
	for rs.Next() {
		var h uint8
		var c uint64
		if rs.Scan(&h, &c) == nil && int(h) < 24 {
			out[h] = c
		}
	}
	return out
}

func (r *FlowReader) rollupRegion(ctx context.Context, ispID uint32, days int) [][]any {
	where, args := scope(ispID, days)
	rs, err := r.conn.Query(ctx, fmt.Sprintf(
		`SELECT device_id, sum(flows) c FROM %s.flow_rollup WHERE %s GROUP BY device_id ORDER BY c DESC LIMIT 7`, r.db, where), args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	var out [][]any
	for rs.Next() {
		var dev uint32
		var c uint64
		if rs.Scan(&dev, &c) == nil {
			out = append(out, []any{fmt.Sprintf("DEV-%d", dev), c})
		}
	}
	return out
}

func (r *FlowReader) rollupTopSubs(ctx context.Context, ispID uint32, days int) []topSub {
	where, args := scope(ispID, days)
	rs, err := r.conn.Query(ctx, fmt.Sprintf(
		`SELECT src_ip, sum(bytes) b FROM %s.flow_rollup_subs WHERE %s GROUP BY src_ip ORDER BY b DESC LIMIT 5`, r.db, where), args...)
	if err != nil {
		return nil
	}
	defer rs.Close()
	type sub struct {
		ip string
		b  uint64
	}
	var subs []sub
	var max uint64
	for rs.Next() {
		var ip net.IP
		var b uint64
		if rs.Scan(&ip, &b) == nil {
			subs = append(subs, sub{ip.String(), b})
			if b > max {
				max = b
			}
		}
	}
	var out []topSub
	for _, s := range subs {
		out = append(out, topSub{Label: s.ip, Value: humanBytes2(s.b), Pct: pctOf(s.b, max)})
	}
	return out
}

func protoMixFrom(tcp, udp, icmp, other uint64) []protoSlice {
	sum := tcp + udp + icmp + other
	if sum == 0 {
		return nil
	}
	colors := map[string]string{"TCP": "#0077b6", "UDP": "#2a9d8f", "ICMP": "#e76f51", "OTH": "#9aa5b1"}
	vals := map[string]uint64{"TCP": tcp, "UDP": udp, "ICMP": icmp, "OTH": other}
	var out []protoSlice
	for _, name := range []string{"TCP", "UDP", "ICMP", "OTH"} {
		if vals[name] > 0 {
			out = append(out, protoSlice{Name: name, Pct: round1(float64(vals[name]) / float64(sum) * 100), Color: colors[name]})
		}
	}
	return out
}
