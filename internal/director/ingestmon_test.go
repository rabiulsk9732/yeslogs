package director

import (
	"testing"
	"time"
)

func snap(decoded, inserted, errs uint64) IngestHealth {
	return IngestHealth{Decoded: decoded, Inserted: inserted, InsertErrors: errs}
}

func TestIngestMonitorStallAndRecovery(t *testing.T) {
	st := &ingestMonState{}
	remind := 6 * time.Hour
	now := time.Unix(1_754_500_000, 0)

	// baseline
	if a, r, _ := evalIngestTick(st, snap(1000, 1000, 0), remind, now); a || r {
		t.Fatal("baseline tick must not alert")
	}
	// healthy ticks
	for i := uint64(1); i <= 3; i++ {
		now = now.Add(time.Minute)
		if a, r, _ := evalIngestTick(st, snap(1000+i*100, 1000+i*100, 0), remind, now); a || r {
			t.Fatal("healthy tick must not alert")
		}
	}
	// stall: decoded grows, inserted frozen — no alert before ingestStallTicks
	base := snap(1300, 1300, 0)
	for i := 1; i < ingestStallTicks; i++ {
		now = now.Add(time.Minute)
		a, r, _ := evalIngestTick(st, snap(base.Decoded+uint64(i)*100, base.Inserted, uint64(i)), remind, now)
		if a || r {
			t.Fatalf("tick %d: alerted before threshold", i)
		}
	}
	now = now.Add(time.Minute)
	a, _, d := evalIngestTick(st, snap(base.Decoded+ingestStallTicks*100, base.Inserted, ingestStallTicks), remind, now)
	if !a {
		t.Fatal("must alert after ingestStallTicks stalled minutes")
	}
	if d.window != ingestStallTicks {
		t.Fatalf("window = %d, want %d", d.window, ingestStallTicks)
	}
	if d.decoded != ingestStallTicks*100 || d.errors != ingestStallTicks {
		t.Fatalf("alert deltas must cover the whole stall window: decoded=%d errors=%d", d.decoded, d.errors)
	}
	// continued stall inside the remind interval: no duplicate alert
	now = now.Add(time.Minute)
	if a, _, _ := evalIngestTick(st, snap(base.Decoded+600, base.Inserted, 6), remind, now); a {
		t.Fatal("must not re-alert inside remind interval")
	}
	// after remind interval elapses, still stalled → reminder alert
	now = now.Add(remind)
	if a, _, _ := evalIngestTick(st, snap(base.Decoded+700, base.Inserted, 7), remind, now); !a {
		t.Fatal("must send reminder after remind interval")
	}
	// inserts resume → recovery notice exactly once
	now = now.Add(time.Minute)
	_, r, _ := evalIngestTick(st, snap(base.Decoded+800, base.Inserted+500, 7), remind, now)
	if !r {
		t.Fatal("must send recovery when inserts resume")
	}
	now = now.Add(time.Minute)
	if a, r, _ := evalIngestTick(st, snap(base.Decoded+900, base.Inserted+600, 7), remind, now); a || r {
		t.Fatal("healthy tick after recovery must be quiet")
	}
}

func TestIngestMonitorErrorsOnlyStall(t *testing.T) {
	// Inserts frozen and batches dying, but decode also frozen (e.g. queue full,
	// receiver back-pressured): errors alone must still count as a stall.
	st := &ingestMonState{}
	now := time.Unix(1_754_500_000, 0)
	evalIngestTick(st, snap(500, 500, 0), 6*time.Hour, now)
	var alerted bool
	for i := uint64(1); i <= ingestStallTicks; i++ {
		now = now.Add(time.Minute)
		a, _, _ := evalIngestTick(st, snap(500, 500, i), 6*time.Hour, now)
		alerted = alerted || a
	}
	if !alerted {
		t.Fatal("insert-error-only stall must alert")
	}
}

func TestIngestMonitorIdleIsQuiet(t *testing.T) {
	// Nothing arriving at all (quiet network / all exporters down): the device
	// monitor owns that case; the ingest monitor must stay silent.
	st := &ingestMonState{}
	now := time.Unix(1_754_500_000, 0)
	evalIngestTick(st, snap(500, 500, 0), 6*time.Hour, now)
	for i := 0; i < ingestStallTicks*3; i++ {
		now = now.Add(time.Minute)
		if a, r, _ := evalIngestTick(st, snap(500, 500, 0), 6*time.Hour, now); a || r {
			t.Fatal("idle ticks must never alert")
		}
	}
}

func TestIngestMonitorRestartRebaseline(t *testing.T) {
	// Counter reset (natlog restart) must re-baseline, not panic on underflow
	// or fire a bogus alert from huge unsigned deltas.
	st := &ingestMonState{}
	now := time.Unix(1_754_500_000, 0)
	evalIngestTick(st, snap(1_000_000, 900_000, 5), 6*time.Hour, now)
	now = now.Add(time.Minute)
	if a, r, _ := evalIngestTick(st, snap(100, 50, 0), 6*time.Hour, now); a || r {
		t.Fatal("restart re-baseline must not alert")
	}
	if st.stalled != 0 {
		t.Fatalf("stalled = %d after re-baseline, want 0", st.stalled)
	}
}

func TestIngestMonitorPartialInsertsHealthy(t *testing.T) {
	// Some batches failing but rows still landing: degraded, not stalled.
	// (Broken-parts / error-rate reporting covers partial failure; the stall
	// alert is reserved for total write outage.)
	st := &ingestMonState{}
	now := time.Unix(1_754_500_000, 0)
	evalIngestTick(st, snap(1000, 1000, 0), 6*time.Hour, now)
	for i := uint64(1); i <= ingestStallTicks*2; i++ {
		now = now.Add(time.Minute)
		if a, _, _ := evalIngestTick(st, snap(1000+i*100, 1000+i*50, i), 6*time.Hour, now); a {
			t.Fatal("partial throughput must not trigger the stall alert")
		}
	}
}

func TestIsBrokenPartsError(t *testing.T) {
	real := `prepare batch: code: 722, message: Waited job failed: Code: 696. DB::Exception: Load job 'startup table natlogs.flow_logs' -> Code: 231. DB::Exception: Suspiciously many (106 parts, 0.00 B in total) broken parts to remove`
	if !isBrokenPartsError(real) {
		t.Fatal("must match the real box3 incident error")
	}
	if isBrokenPartsError("dial tcp 127.0.0.1:9000: connect: connection refused") {
		t.Fatal("plain connection error must not match")
	}
}
