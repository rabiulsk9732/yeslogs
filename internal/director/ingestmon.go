package director

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// IngestHealth is a cumulative ingest snapshot supplied by the host (natlog):
// monotonic counters since process start plus the writer's most recent insert
// error. The monitor works on deltas between ticks, so absolute values only
// need to be consistent with each other.
type IngestHealth struct {
	Decoded      uint64 // flows decoded since start
	Inserted     uint64 // flows written to ClickHouse since start
	InsertErrors uint64 // batches dropped after exhausting retries since start
	LastError    string // most recent insert failure ("" if none yet)
	LastErrorAt  time.Time
}

// SetIngestHealth registers the ingest snapshot provider. Call before
// RunIngestMonitor.
func (s *Server) SetIngestHealth(f func() IngestHealth) { s.ingestFn = f }

const (
	ingestTickEvery  = time.Minute
	ingestStallTicks = 5  // consecutive stalled minutes before alerting
	brokenCheckEvery = 10 // check for broken detached parts every N ticks
)

// ingestMonState is the monitor's memory between ticks. It lives inside the
// RunIngestMonitor goroutine only — no locking needed.
type ingestMonState struct {
	prev     IngestHealth
	havePrev bool
	stalled  int  // consecutive stalled ticks
	alerted  bool // an ingest-down alert is outstanding (recovery pending)
	// totals across the current stall, so alert emails report the whole
	// window, not just the last minute's delta
	stallDecoded, stallErrors uint64

	lastAlert       time.Time
	broken          uint64 // last observed broken detached-parts count
	lastBrokenAlert time.Time
	dbDown          bool
	lastDBAlert     time.Time
}

// RunIngestMonitor watches writer throughput and alerts when flows keep
// arriving but nothing reaches ClickHouse (DB down, table refusing to load,
// writer wedged). This is the "silent 4-day outage" guard: every minute of
// stall is flow data lost forever, so it must page someone. Safe to start
// unconditionally; no-ops until SetIngestHealth and a notifier are wired.
func (s *Server) RunIngestMonitor(ctx context.Context) {
	if s.ingestFn == nil {
		return
	}
	tick := time.NewTicker(ingestTickEvery)
	defer tick.Stop()
	st := &ingestMonState{}
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.clickHouseHealthCheck(ctx, st)
			s.ingestTick(ctx, st)
			if n++; n%brokenCheckEvery == 0 {
				s.brokenPartsCheck(ctx, st)
			}
		}
	}
}

// clickHouseHealthCheck catches a database crash even during a quiet period,
// when the decoded/inserted delta monitor has no traffic signal to compare.
func (s *Server) clickHouseHealthCheck(ctx context.Context, st *ingestMonState) {
	if s.flows == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err := s.flows.Ping(qctx)
	cancel()
	now := time.Now()
	if err == nil && !st.dbDown {
		return
	}
	notifier := s.getNotifier()
	enabled := s.CurrentSettings().Notifications.Enabled
	name := s.stats().Name
	if err == nil {
		st.dbDown = false
		if notifier == nil || !enabled {
			return
		}
		sctx, sc := context.WithTimeout(ctx, 30*time.Second)
		sendErr := notifier(sctx, fmt.Sprintf("[ISPmate] RECOVERED: ClickHouse is reachable on %s", name), fmt.Sprintf("ClickHouse connectivity has recovered on dataplane %q. Verify the writer spool drains and per-day counts remain continuous.\n\n— ISPmate Operations", name))
		sc()
		if sendErr != nil {
			s.log.Error("ClickHouse recovery alert failed", "error", sendErr)
		}
		return
	}
	st.dbDown = true
	if notifier == nil || !enabled || (!st.lastDBAlert.IsZero() && now.Sub(st.lastDBAlert) < s.remindInterval()) {
		return
	}
	sctx, sc := context.WithTimeout(ctx, 30*time.Second)
	sendErr := notifier(sctx, fmt.Sprintf("[ISPmate] CLICKHOUSE DOWN on %s", name), fmt.Sprintf("ClickHouse health check failed on dataplane %q:\n\n%s\n\nNew batches should remain in the configured local spool until recovery. Check clickhouse-server and disk health immediately.\n\n— ISPmate Operations", name, err))
	sc()
	if sendErr != nil {
		s.log.Error("ClickHouse down alert failed", "error", sendErr)
		return
	}
	st.lastDBAlert = now
	s.log.Error("ClickHouse down alert sent", "error", err)
}

func (s *Server) ingestTick(ctx context.Context, st *ingestMonState) {
	cur := s.ingestFn()
	alert, recovered, deltas := evalIngestTick(st, cur, s.remindInterval(), time.Now())
	if !alert && !recovered {
		return
	}
	notifier := s.getNotifier()
	if notifier == nil || !s.CurrentSettings().Notifications.Enabled {
		return // state machine still advanced; emails just aren't deliverable/wanted
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	name := s.stats().Name
	if recovered {
		subject := fmt.Sprintf("[ISPmate] RECOVERED: ClickHouse inserts resumed on %s", name)
		body := fmt.Sprintf("Flow inserts to ClickHouse have resumed on dataplane %q.\n\nVerify there is no gap: check Retention → per-day counts around this time.\n\n— ISPmate Operations", name)
		if err := notifier(sctx, subject, body); err != nil {
			s.log.Error("ingest recovery email failed", "error", err)
			return
		}
		s.log.Info("ingest recovery alert sent")
		return
	}
	subject := fmt.Sprintf("[ISPmate] INGEST STALLED on %s — flows arriving but NOT stored", name)
	body := ingestAlertBody(name, deltas, cur)
	if err := notifier(sctx, subject, body); err != nil {
		s.log.Error("ingest stall email failed", "error", err)
		return
	}
	s.log.Error("ingest stall alert sent", "decoded", deltas.decoded, "inserted", deltas.inserted, "insertErrors", deltas.errors)
}

func (s *Server) remindInterval() time.Duration {
	h := s.CurrentSettings().Notifications.RemindHours
	if h < 1 {
		h = 6
	}
	return time.Duration(h) * time.Hour
}

type ingestDeltas struct {
	decoded, inserted, errors uint64
	window                    int // stalled minutes covered by the alert
}

// evalIngestTick advances the monitor state with a fresh snapshot and reports
// whether a stall alert or a recovery notice is due. Pure — unit-testable.
func evalIngestTick(st *ingestMonState, cur IngestHealth, remind time.Duration, now time.Time) (alert, recovered bool, d ingestDeltas) {
	if !st.havePrev || cur.Decoded < st.prev.Decoded || cur.Inserted < st.prev.Inserted {
		// First sample, or the process restarted (counters reset): re-baseline.
		st.prev, st.havePrev, st.stalled = cur, true, 0
		st.stallDecoded, st.stallErrors = 0, 0
		return false, false, d
	}
	d = ingestDeltas{decoded: cur.Decoded - st.prev.Decoded, inserted: cur.Inserted - st.prev.Inserted, errors: cur.InsertErrors - st.prev.InsertErrors}
	st.prev = cur
	switch {
	case d.inserted == 0 && (d.decoded > 0 || d.errors > 0):
		// Work arrived (or batches died) and nothing landed in the DB.
		st.stalled++
		st.stallDecoded += d.decoded
		st.stallErrors += d.errors
		if st.stalled >= ingestStallTicks && (!st.alerted || now.Sub(st.lastAlert) >= remind) {
			st.alerted, st.lastAlert = true, now
			d.window, d.decoded, d.errors = st.stalled, st.stallDecoded, st.stallErrors
			return true, false, d
		}
	case d.inserted > 0:
		st.stalled = 0
		st.stallDecoded, st.stallErrors = 0, 0
		if st.alerted {
			st.alerted = false
			return false, true, d
		}
	}
	// else: fully idle tick (nothing decoded/inserted/failed) — keep state as is;
	// device-down alerting owns the "no flows arriving at all" case.
	return false, false, d
}

func ingestAlertBody(name string, d ingestDeltas, cur IngestHealth) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dataplane %q is receiving flows but NOTHING is being written to ClickHouse.\n\n", name)
	fmt.Fprintf(&b, "Over the last %d minutes: decoded %d flows, inserted 0, failed batches %d.\n", d.window, d.decoded, d.errors)
	fmt.Fprintf(&b, "Every minute this persists is flow data lost forever — NetFlow/IPFIX is not retransmitted.\n\n")
	if cur.LastError != "" {
		when := ""
		if !cur.LastErrorAt.IsZero() {
			when = " (" + cur.LastErrorAt.In(istLoc).Format("2006-01-02 15:04:05 IST") + ")"
		}
		fmt.Fprintf(&b, "Last insert error%s:\n  %s\n\n", when, cur.LastError)
		if isBrokenPartsError(cur.LastError) {
			b.WriteString(`This looks like ClickHouse refusing to load a table after an unclean
shutdown/power loss (broken parts). Runbook:
  1. systemctl status clickhouse-server && journalctl -u clickhouse-server -n 50
  2. clickhouse client -q "SELECT database, table, count() FROM system.detached_parts WHERE name LIKE 'broken%' GROUP BY database, table"
  3. If a rollup table (flow_rollup*) is what refuses to load, it is REBUILDABLE:
     DETACH/DROP it so flow_logs ingest resumes, then rebuild it from flow_logs.
  4. Never leave this overnight — ingest is down while it persists.

`)
		}
	}
	b.WriteString("Check: systemctl status natlog clickhouse-server; journalctl -u natlog -n 50\n\n— ISPmate Operations")
	return b.String()
}

// isBrokenPartsError matches the ClickHouse error chain seen when broken parts
// block a table's startup load job (TOO_MANY_UNEXPECTED_DATA_PARTS et al).
func isBrokenPartsError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "broken part") || strings.Contains(m, "too_many_unexpected_data_parts") ||
		strings.Contains(m, "suspicious") || strings.Contains(m, "load job")
}

// brokenPartsCheck alerts when ClickHouse has silently set aside broken parts
// (max_suspicious_broken_parts keeps the table loading, but those rows are
// gone from hot storage — the operator must know).
func (s *Server) brokenPartsCheck(ctx context.Context, st *ingestMonState) {
	if s.flows == nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	n, err := s.flows.BrokenParts(qctx)
	cancel()
	if err != nil {
		s.log.Warn("broken-parts check failed", "error", err)
		return
	}
	prev := st.broken
	if n <= prev { // shrink/steady (operator cleaned up): just track it
		st.broken = n
		return
	}
	// Growth. Leave st.broken at prev until an alert actually goes out, so a
	// gated/failed send is retried on the next check instead of lost forever.
	now := time.Now()
	if !st.lastBrokenAlert.IsZero() && now.Sub(st.lastBrokenAlert) < s.remindInterval() {
		return
	}
	notifier := s.getNotifier()
	if notifier == nil || !s.CurrentSettings().Notifications.Enabled {
		return
	}
	name := s.stats().Name
	subject := fmt.Sprintf("[ISPmate] DATA WARNING on %s: %d broken ClickHouse parts detached", name, n)
	body := fmt.Sprintf(`ClickHouse on dataplane %q has detached %d broken data parts (was %d).

These parts were corrupted (typically by an unclean shutdown/power loss) and
their rows are NO LONGER in hot storage. Ingest keeps running, but this is
silent data loss — investigate now:

  clickhouse client -q "SELECT database, table, name, disk FROM system.detached_parts WHERE name LIKE 'broken%%' LIMIT 50"

If the affected table is a rollup (flow_rollup*), rebuild it from flow_logs.
If flow_logs itself is affected, quantify the gap (Retention → per-day counts)
and check the S3 cold archive for those days.

— ISPmate Operations`, name, n, prev)
	sctx, sc := context.WithTimeout(ctx, 30*time.Second)
	defer sc()
	if err := notifier(sctx, subject, body); err != nil {
		s.log.Error("broken-parts email failed", "error", err)
		return // st.broken stays at prev → retried on the next check
	}
	st.broken = n
	st.lastBrokenAlert = now
	s.log.Error("broken-parts alert sent", "count", n, "previous", prev)
}
