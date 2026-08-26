package spool

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

func batch(n int, port uint16) []normalizer.FlowRecord {
	out := make([]normalizer.FlowRecord, n)
	for i := range out {
		out[i] = normalizer.FlowRecord{
			ISPID: 3, DeviceID: 2,
			SrcIP: net.ParseIP("100.64.12.9"), SrcPort: port + uint16(i),
			DstIP: net.ParseIP("142.251.42.14"), DstPort: 443,
			NatPublicIP: net.ParseIP("103.204.1.14"), NatPublicPort: 40112 + uint16(i),
			Protocol: 6, FlowStart: time.Unix(1787000000, 0).UTC(), FlowEnd: time.Unix(1787000001, 0).UTC(),
			FlowType: "netflow9", ExporterIP: net.ParseIP("103.204.1.14"),
		}
	}
	return out
}

// The whole point: a batch survives the process that wrote it. Anything less and
// the eight days lost on 2026-08-01 would be lost again.
func TestBatchSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := batch(3, 50000)
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	// A completely fresh Spool over the same directory — as after a restart.
	s2, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if files, _, _ := s2.Stats(); files != 1 {
		t.Fatalf("restart found %d files, want 1", files)
	}
	name, got, ok, err := s2.Oldest()
	if err != nil || !ok {
		t.Fatalf("Oldest: ok=%v err=%v", ok, err)
	}
	if len(got) != len(want) {
		t.Fatalf("recovered %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].NatPublicIP.Equal(want[i].NatPublicIP) || got[i].NatPublicPort != want[i].NatPublicPort {
			t.Fatalf("record %d came back changed: %v:%d vs %v:%d", i,
				got[i].NatPublicIP, got[i].NatPublicPort, want[i].NatPublicIP, want[i].NatPublicPort)
		}
		if !got[i].FlowStart.Equal(want[i].FlowStart) {
			t.Errorf("record %d timestamp changed: %v vs %v", i, got[i].FlowStart, want[i].FlowStart)
		}
	}
	if err := s2.Done(name); err != nil {
		t.Fatal(err)
	}
	if files, _, _ := s2.Stats(); files != 0 {
		t.Errorf("after Done: %d files, want 0", files)
	}
}

// A half-written file is a write that never completed, so its batch was never
// acknowledged as spooled. Replaying a truncated batch would insert partial
// evidence, which is worse than replaying none.
func TestUnfinishedWriteIsDiscardedOnStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000009.spool.tmp"), []byte("half"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if files, _, _ := s.Stats(); files != 0 {
		t.Errorf("a .tmp file was adopted as a real batch: %d files", files)
	}
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000009.spool.tmp")); !os.IsNotExist(err) {
		t.Error("the partial file should have been removed")
	}
}

// Oldest first: the longest-waiting evidence should land soonest.
func TestOldestBatchComesBackFirst(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 1<<30)
	for i, p := range []uint16{1000, 2000, 3000} {
		if err := s.Save(batch(1, p)); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	for _, wantPort := range []uint16{1000, 2000, 3000} {
		name, got, ok, err := s.Oldest()
		if err != nil || !ok {
			t.Fatalf("Oldest: ok=%v err=%v", ok, err)
		}
		if got[0].SrcPort != wantPort {
			t.Fatalf("got batch starting at port %d, want %d", got[0].SrcPort, wantPort)
		}
		if err := s.Done(name); err != nil {
			t.Fatal(err)
		}
	}
}

// The cap must hold. Filling the disk would take the collector down and cost
// every exporter, which is worse than losing one batch.
func TestSaveRefusesPastTheCap(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 1) // 1 byte: anything already written exceeds it
	if err := s.Save(batch(1, 100)); err != nil {
		t.Fatalf("first save should succeed (cap is checked before writing): %v", err)
	}
	if err := s.Save(batch(1, 200)); err != ErrFull {
		t.Fatalf("second save = %v, want ErrFull", err)
	}
	if !s.MarkFull() {
		t.Error("MarkFull should report the first transition into the full state")
	}
	if s.MarkFull() {
		t.Error("MarkFull should not re-report while still full")
	}
}

// A batch that cannot be inserted must not block every batch behind it, and must
// not be deleted either — it is evidence.
func TestQuarantineKeepsTheDataAndUnblocksTheQueue(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 1<<30)
	if err := s.Save(batch(2, 7000)); err != nil {
		t.Fatal(err)
	}
	name, _, _, _ := s.Oldest()
	if err := s.Quarantine(name); err != nil {
		t.Fatal(err)
	}
	if files, _, _ := s.Stats(); files != 0 {
		t.Errorf("quarantined batch still counted as pending: %d files", files)
	}
	if _, _, ok, _ := s.Oldest(); ok {
		t.Error("quarantined batch is still being offered for replay")
	}
	if _, err := os.Stat(filepath.Join(dir, "quarantine", name)); err != nil {
		t.Errorf("quarantined batch was not kept on disk: %v", err)
	}
}

// An empty batch is not an error and must not create a file.
func TestEmptyBatchWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, 1<<30)
	if err := s.Save(nil); err != nil {
		t.Fatal(err)
	}
	if files, _, _ := s.Stats(); files != 0 {
		t.Errorf("empty batch created %d files", files)
	}
}
