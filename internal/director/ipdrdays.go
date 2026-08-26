package director

import (
	"context"
	"fmt"
	"time"
)

// Per-day IPDR answerability.
//
// The compliance audit needs one number per retained day: how many of that day's
// stored flows carry a NAT translation, and so could answer a lawful request.
// Computing it reads two IP columns across the day, which is fine for one day
// (measured 1.5-6.4s on the busiest collector in the fleet) and hopeless for the
// whole window at once (>170s over 51 days, and ClickHouse rejects it up front as
// too slow). The audit runs every 15 minutes, so it cannot pay that cost inline.
//
// So it is computed once per day and stored. flow_ipdr_days holds one row per
// (isp, date) and is read in microseconds.
//
// The table is deliberately NOT part of flow_rollup. That one is an
// AggregatingMergeTree keyed by (isp, date, hour, device) whose columns SUM on
// merge, so re-running a day would permanently double its counters — exactly the
// drift RollupTick's watermark exists to prevent. This table replaces instead of
// sums, which makes recomputing a day safe and therefore makes the whole
// self-healing loop below possible.
//
//	flow_ipdr_days — per (isp, day): stored flows and how many were translated

const ipdrDaysTable = "flow_ipdr_days"

// ipdrDayBudget bounds one backfill pass. Enough to walk a full 180-day window
// in well under an hour of ticks, few enough that the pass never competes with
// ingest for long on a collector that is also writing 200M rows a day.
const ipdrDayBudget = 4

// ipdrDayTimeout bounds a single day's scan. Generous against the 6.4s worst
// case measured, short enough that a pathological day is abandoned rather than
// blocking the rest of the backlog behind it.
const ipdrDayTimeout = 90 * time.Second

// ipdrDayTickEvery is how often the backfill looks for work. Most ticks find
// only today (still growing) and finish in seconds.
const ipdrDayTickEvery = 5 * time.Minute

// ipdrDayRow is one stored (isp, date) row.
type ipdrDayRow struct {
	Flows      uint64
	Translated uint64
}

func ipdrDaysDDL(db string) string {
	// ReplacingMergeTree keyed on (isp_id, event_date) with computed_at as the
	// version: recomputing a day inserts a new row that supersedes the old one on
	// merge, and argMax(…, computed_at) reads the newest before merges catch up.
	// One row per isp per day, so the table stays in the low thousands forever.
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    isp_id UInt32, event_date Date,
    flows UInt64, translated UInt64,
    computed_at DateTime DEFAULT now()
) ENGINE = ReplacingMergeTree(computed_at) ORDER BY (isp_id, event_date)`, db, ipdrDaysTable)
}

// EnsureIPDRDays creates the per-day table. Idempotent.
func (r *FlowReader) EnsureIPDRDays(ctx context.Context) error {
	if err := r.conn.Exec(ctx, ipdrDaysDDL(r.db)); err != nil {
		return fmt.Errorf("ipdr days ddl: %w", err)
	}
	return nil
}

// storedIPDRDays reads the computed per-day figures, newest first. ispID 0 sums
// across tenants, matching the director-scope view of every other count.
func (r *FlowReader) storedIPDRDays(ctx context.Context, ispID uint32) (map[string]ipdrDayRow, error) {
	where, args := "1", []any(nil)
	if ispID != 0 {
		where, args = "isp_id = ?", []any{ispID}
	}
	// argMax over computed_at rather than FINAL: same answer, no merge-time
	// penalty, and correct even with an unmerged duplicate sitting in a new part.
	q := fmt.Sprintf(`SELECT toString(event_date), sum(f), sum(t) FROM (
			SELECT event_date, isp_id, argMax(flows, computed_at) f, argMax(translated, computed_at) t
			FROM %s.%s WHERE %s GROUP BY event_date, isp_id)
		GROUP BY event_date`, r.db, ipdrDaysTable, where)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ipdrDayRow{}
	for rows.Next() {
		var date string
		var v ipdrDayRow
		if err := rows.Scan(&date, &v.Flows, &v.Translated); err != nil {
			return nil, err
		}
		out[date] = v
	}
	return out, rows.Err()
}

// computeIPDRDay measures one day and stores the result for every tenant that
// has data on it. Safe to re-run: the row replaces rather than accumulates.
func (r *FlowReader) computeIPDRDay(ctx context.Context, date string) error {
	cctx, cancel := context.WithTimeout(ctx, ipdrDayTimeout)
	defer cancel()
	q := fmt.Sprintf(`INSERT INTO %s.%s (isp_id, event_date, flows, translated, computed_at)
		SELECT isp_id, event_date, count(),
			countIf(nat_public_ip != toIPv4('0.0.0.0') AND nat_public_ip != src_ip), now()
		FROM %s.flow_logs WHERE event_date = ? GROUP BY isp_id, event_date
		SETTINGS max_execution_time = %d`, r.db, ipdrDaysTable, r.db, int(ipdrDayTimeout.Seconds())-5)
	return r.conn.Exec(cctx, q, date)
}

// ipdrDaysNeedingWork returns the dates whose stored figures are missing or out
// of date, newest first.
//
// Staleness is detected by comparing the stored flow count against the live one.
// The live count is free — it reads only the partition key — so every day can be
// checked on every tick, and a day that grew (today, or an old day that received
// late-arriving records) is recomputed automatically. That is what keeps this
// self-healing rather than a one-shot migration whose output silently rots.
func ipdrDaysNeedingWork(live []DayIPDR, stored map[string]ipdrDayRow, budget int) []string {
	var out []string
	for _, d := range live {
		if len(out) >= budget {
			break
		}
		if s, ok := stored[d.Date]; !ok || s.Flows != d.Flows {
			out = append(out, d.Date)
		}
	}
	return out
}

// RunIPDRDayBackfill keeps the per-day translation figures current: today, every
// tick, because it is still growing; and the rest of the retention window a few
// days at a time until the backlog is gone. On a fresh 180-day store the whole
// window fills within an hour of ticks, and after that most passes do one day.
func (s *Server) RunIPDRDayBackfill(ctx context.Context) {
	if s.flows == nil {
		return
	}
	if err := s.flows.EnsureIPDRDays(ctx); err != nil {
		s.log.Warn("ipdr days: table unavailable; per-day answerability will read as unmeasured", "error", err)
		return
	}
	tick := time.NewTicker(ipdrDayTickEvery)
	defer tick.Stop()
	first := time.NewTimer(30 * time.Second)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
			s.ipdrDayPass(ctx)
		case <-tick.C:
			s.ipdrDayPass(ctx)
		}
	}
}

func (s *Server) ipdrDayPass(ctx context.Context) {
	// Director scope: the table is written per tenant, so one pass covers all.
	live, err := s.flows.dayFlowCounts(ctx, 0)
	if err != nil {
		s.log.Warn("ipdr days: day counts failed", "error", err)
		return
	}
	stored, err := s.flows.storedIPDRDays(ctx, 0)
	if err != nil {
		s.log.Warn("ipdr days: stored figures unreadable", "error", err)
		return
	}
	work := ipdrDaysNeedingWork(live, stored, ipdrDayBudget)
	if len(work) == 0 {
		return
	}
	start := time.Now()
	done := 0
	for _, date := range work {
		if err := s.flows.computeIPDRDay(ctx, date); err != nil {
			s.log.Warn("ipdr days: day not measured", "date", date, "error", err)
			continue
		}
		done++
	}
	// Say what is left, not just what was done: a backlog that stops shrinking is
	// the signal that a day is failing every pass, and a silent worker hides it.
	remaining := len(ipdrDaysNeedingWork(live, stored, len(live))) - done
	s.log.Info("ipdr days: measured", "days", done, "remaining", max(remaining, 0), "took", time.Since(start).Round(time.Millisecond).String())
}
