package spool

import (
	"crypto/sha256"
	"encoding/gob"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

func TestReplayLegacyBatchWithoutDestinationFields(t *testing.T) {
	dir := t.TempDir()
	// Gob maps by field name. This is the pre-upgrade record shape rather than
	// a new FlowRecord with zero-valued fields, so absent-field compatibility is
	// exercised across an actual on-disk decode.
	legacy := []struct {
		ISPID         uint32
		DeviceID      uint32
		SrcIP         net.IP
		SrcPort       uint16
		NatPublicIP   net.IP
		NatPublicPort uint16
	}{{5, 8, net.ParseIP("10.0.102.12"), 42286, net.ParseIP("203.0.113.1"), 52286}}
	f, err := os.Create(filepath.Join(dir, "00000000000000000001.spool"))
	if err != nil {
		t.Fatal(err)
	}
	err = gob.NewEncoder(f).Encode(legacy)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	_, got, ok, err := s.Oldest()
	if err != nil || !ok || len(got) != 1 {
		t.Fatalf("legacy replay: ok=%v rows=%d err=%v", ok, len(got), err)
	}
	if got[0].ISPID != 5 || got[0].DeviceID != 8 || got[0].SrcPort != 42286 || got[0].NatPublicPort != 52286 ||
		!got[0].NatPublicIP.Equal(legacy[0].NatPublicIP) || got[0].NatDestIP != nil || got[0].NatDestPort != 0 {
		t.Fatalf("legacy evidence changed on replay: %+v", got[0])
	}
}

func batch(n int, port uint16) []normalizer.FlowRecord {
	out := make([]normalizer.FlowRecord, n)
	for i := range out {
		out[i] = normalizer.FlowRecord{
			ISPID: 3, DeviceID: 2,
			SrcIP: net.ParseIP("100.64.12.9"), SrcPort: port + uint16(i),
			DstIP: net.ParseIP("142.251.42.14"), DstPort: 443,
			NatPublicIP: net.ParseIP("103.204.1.14"), NatPublicPort: 40112 + uint16(i),
			NatDestIP: net.ParseIP("10.0.102.12"), NatDestPort: 42286 + uint16(i),
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
		if !got[i].NatDestIP.Equal(want[i].NatDestIP) || got[i].NatDestPort != want[i].NatDestPort {
			t.Fatalf("record %d post-NAT destination changed during spool replay", i)
		}
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

// A live writer owns an active WAL batch, so the replay goroutine must not race
// it and insert the same rows concurrently. Once released, it becomes the oldest
// replayable batch without changing its durable contents.
func TestWriteAheadBatchHiddenUntilReady(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := batch(2, 51000)
	name, err := s.Begin(want)
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatal("Begin returned an empty WAL handle")
	}
	if _, _, ok, err := s.Oldest(); err != nil || ok {
		t.Fatalf("active batch leaked into replay: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name+activeExt)); err != nil {
		t.Fatalf("active WAL file missing: %v", err)
	}
	if files, _, _ := s.Stats(); files != 1 {
		t.Fatalf("active WAL accounting = %d files, want 1", files)
	}

	if err := s.Ready(name); err != nil {
		t.Fatal(err)
	}
	gotName, got, ok, err := s.Oldest()
	if err != nil || !ok {
		t.Fatalf("released batch not replayable: ok=%v err=%v", ok, err)
	}
	if gotName != name || len(got) != len(want) || got[0].SrcPort != want[0].SrcPort {
		t.Fatalf("released batch changed: name=%q rows=%d first_port=%d", gotName, len(got), got[0].SrcPort)
	}
}

// A process can die after the WAL fsync and before the ClickHouse response. A
// fresh process must adopt that active file and replay it (at-least-once).
func TestRestartAdoptsActiveWriteAheadBatch(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	want := batch(3, 52000)
	name, err := s.Begin(want)
	if err != nil {
		t.Fatal(err)
	}

	s2, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	gotName, got, ok, err := s2.Oldest()
	if err != nil || !ok {
		t.Fatalf("restart did not adopt active WAL: ok=%v err=%v", ok, err)
	}
	if gotName != name || len(got) != len(want) || got[2].SrcPort != want[2].SrcPort {
		t.Fatalf("adopted WAL changed: name=%q rows=%d", gotName, len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, name+activeExt)); !os.IsNotExist(err) {
		t.Fatalf("active name survived adoption: %v", err)
	}
}

func TestRestartRefusesAmbiguousActiveAndReadyCopies(t *testing.T) {
	dir := t.TempDir()
	name := "00000000000000000007.spool"
	for _, suffix := range []string{"", activeExt} {
		f, err := os.Create(filepath.Join(dir, name+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if err := encodeBatch(f, batch(1, 52500)); err != nil {
			f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New(dir, 1<<30); err == nil {
		t.Fatal("ambiguous active/ready WAL copies should stop startup")
	}
}

func TestDedupTokenIdentityIsStableAndFleetUnique(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	a1, err := New(dirA, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := New(dirA, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(dirB, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	name := "00000000000000000001.spool"
	if a1.DedupToken(name) != a2.DedupToken(name) {
		t.Fatal("WAL identity changed across restart")
	}
	if a1.DedupToken(name) == b.DedupToken(name) {
		t.Fatal("different collectors generated a colliding WAL dedup token")
	}
	if a1.DedupToken("") != "" {
		t.Fatal("empty WAL handle should not create a dedup token")
	}
}

func TestSuccessfulInsertRemovesActiveWriteAheadBatch(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.Begin(batch(1, 53000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Done(name); err != nil {
		t.Fatal(err)
	}
	if files, bytes, _ := s.Stats(); files != 0 || bytes != 0 {
		t.Fatalf("committed WAL still accounted: files=%d bytes=%d", files, bytes)
	}
	if _, err := os.Stat(filepath.Join(dir, name+activeExt)); !os.IsNotExist(err) {
		t.Fatalf("committed active WAL still exists: %v", err)
	}
}

func TestWriteAheadChecksumDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	name, err := s.Begin(batch(4, 54000))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(name); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= len(walHeader)+sha256.Size+4 {
		t.Fatalf("WAL unexpectedly short: %d", len(b))
	}
	// Change the gob payload while leaving the stored digest untouched.
	b[len(walHeader)+4] ^= 0xff
	if err := os.WriteFile(p, b, 0o640); err != nil {
		t.Fatal(err)
	}
	gotName, _, ok, err := s.Oldest()
	if err == nil || ok || gotName != name {
		t.Fatalf("corrupt WAL accepted: name=%q ok=%v err=%v", gotName, ok, err)
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

func TestQuarantineStillCountsAgainstDiskCapAndSequence(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1) // first batch may cross cap; retained evidence consumes it
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(batch(1, 7100)); err != nil {
		t.Fatal(err)
	}
	name, _, _, _ := s.Oldest()
	if err := s.Quarantine(name); err != nil {
		t.Fatal(err)
	}
	files, bytes, atCap := s.Stats()
	if files != 0 || bytes == 0 || !atCap {
		t.Fatalf("quarantine accounting: files=%d bytes=%d atCap=%v", files, bytes, atCap)
	}
	if err := s.Save(batch(1, 7200)); err != ErrFull {
		t.Fatalf("quarantined evidence bypassed disk cap: %v", err)
	}

	// Restart must retain both the cap accounting and the maximum sequence so a
	// future quarantine can never overwrite the existing evidence file.
	s2, err := New(dir, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if _, bytes2, _ := s2.Stats(); bytes2 != bytes {
		t.Fatalf("restart forgot quarantine bytes: got %d want %d", bytes2, bytes)
	}
	name2, err := s2.Begin(batch(1, 7300))
	if err != nil {
		t.Fatal(err)
	}
	if seqOf(name2) <= seqOf(name) {
		t.Fatalf("sequence reused after quarantine: old=%s new=%s", name, name2)
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

func BenchmarkWriteAheadBeginDone5000(b *testing.B) {
	s, err := New(b.TempDir(), 1<<40)
	if err != nil {
		b.Fatal(err)
	}
	records := batch(5000, 10000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name, err := s.Begin(records)
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Done(name); err != nil {
			b.Fatal(err)
		}
	}
}
