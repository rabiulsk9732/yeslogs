// Package spool gives the ClickHouse writer somewhere to put records it cannot
// insert right now, so an outage costs time instead of evidence.
//
// This exists because the collector has twice destroyed records it had already
// received and decoded. On 2026-08-01 an unclean shutdown left 195 broken parts,
// ClickHouse refused to attach flow_logs at all, and natlog kept accepting flows
// and discarding every batch for eight days — 2026-07-27 to 2026-08-03 is gone
// from box 1 for all three of its ISPs, in hot storage and in S3 alike. On
// 2026-08-26 the same shape of fault cost box 3 about 28.6 million records in
// three hours. In both cases the records existed in memory, fully decoded, and
// were thrown away because the database was briefly unavailable.
//
// Flow export is fire-and-forget: nothing retransmits, so a batch dropped here
// is a lawful request that can never be answered. Three retries over a couple of
// seconds is not a durability story. Writing the batch to disk is.
package spool

import (
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/natflow/natflow-dataplane/internal/normalizer"
)

const (
	fileExt = ".spool"
	tmpExt  = ".tmp"
)

// Spool is a bounded, crash-safe directory of batches awaiting insert.
//
// Ordering is by filename, which embeds a monotonic sequence, so replay is
// roughly first-in-first-out. Exact ordering does not matter — every record
// carries its own timestamps and the table is not append-ordered — but oldest
// first means the longest-waiting evidence lands soonest.
type Spool struct {
	dir      string
	maxBytes int64

	mu    sync.Mutex
	seq   uint64
	bytes int64
	files int

	// full latches once the cap is reached so the log says so exactly once per
	// episode rather than on every batch.
	full atomic.Bool
}

// New prepares dir and adopts any batches left there by a previous run — which
// is the entire point: a spool that forgot its contents on restart would lose
// exactly the records it was created to protect.
func New(dir string, maxBytes int64) (*Spool, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("spool dir not set")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("spool mkdir %s: %w", dir, err)
	}
	s := &Spool{dir: dir, maxBytes: maxBytes}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("spool read %s: %w", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		// A .tmp file is a write that did not finish; its batch was never
		// acknowledged as spooled, so it is incomplete and must not be replayed.
		if strings.HasSuffix(name, tmpExt) {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if !strings.HasSuffix(name, fileExt) {
			continue
		}
		fi, ferr := e.Info()
		if ferr != nil {
			continue
		}
		s.bytes += fi.Size()
		s.files++
		if n := seqOf(name); n > s.seq {
			s.seq = n
		}
	}
	return s, nil
}

func seqOf(name string) uint64 {
	var n uint64
	_, err := fmt.Sscanf(name, "%020d"+fileExt, &n)
	if err != nil {
		return 0
	}
	return n
}

// Save writes one batch durably. It returns ErrFull when the cap is reached:
// the caller must then drop and count, because filling the disk would take the
// collector down entirely and cost every exporter rather than one batch.
func (s *Spool) Save(batch []normalizer.FlowRecord) error {
	if len(batch) == 0 {
		return nil
	}
	s.mu.Lock()
	if s.maxBytes > 0 && s.bytes >= s.maxBytes {
		s.mu.Unlock()
		return ErrFull
	}
	s.seq++
	name := fmt.Sprintf("%020d%s", s.seq, fileExt)
	s.mu.Unlock()

	final := filepath.Join(s.dir, name)
	tmp := final + tmpExt
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("spool create: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(batch); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("spool encode: %w", err)
	}
	// fsync before rename: the fault this guards against is an unclean shutdown,
	// and a batch that only reached the page cache would not survive one.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("spool sync: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("spool close: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("spool rename: %w", err)
	}
	if fi, serr := os.Stat(final); serr == nil {
		s.mu.Lock()
		s.bytes += fi.Size()
		s.files++
		s.mu.Unlock()
	}
	s.full.Store(false)
	return nil
}

// ErrFull reports that the spool has reached its configured size cap.
var ErrFull = fmt.Errorf("spool full")

// Oldest returns the longest-waiting batch and the handle to release it. It
// returns ok=false when the spool is empty.
func (s *Spool) Oldest() (name string, batch []normalizer.FlowRecord, ok bool, err error) {
	entries, rerr := os.ReadDir(s.dir)
	if rerr != nil {
		return "", nil, false, rerr
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), fileExt) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil, false, nil
	}
	sort.Strings(names) // zero-padded sequence sorts chronologically
	name = names[0]
	f, oerr := os.Open(filepath.Join(s.dir, name))
	if oerr != nil {
		return name, nil, false, oerr
	}
	defer f.Close()
	if derr := gob.NewDecoder(f).Decode(&batch); derr != nil && derr != io.EOF {
		return name, nil, false, fmt.Errorf("spool decode %s: %w", name, derr)
	}
	return name, batch, true, nil
}

// Done removes a batch that was successfully inserted.
func (s *Spool) Done(name string) error {
	p := filepath.Join(s.dir, name)
	fi, err := os.Stat(p)
	if err == nil {
		s.mu.Lock()
		s.bytes -= fi.Size()
		if s.bytes < 0 {
			s.bytes = 0
		}
		s.files--
		if s.files < 0 {
			s.files = 0
		}
		s.mu.Unlock()
	}
	return os.Remove(p)
}

// Quarantine moves a batch aside after repeated failures so one undecodable or
// permanently rejected file cannot block every batch behind it forever. It stays
// on disk: this is evidence, and deleting it to unblock a queue would repeat the
// mistake this package exists to correct.
func (s *Spool) Quarantine(name string) error {
	src := filepath.Join(s.dir, name)
	dstDir := filepath.Join(s.dir, "quarantine")
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	if fi, err := os.Stat(src); err == nil {
		s.mu.Lock()
		s.bytes -= fi.Size()
		if s.bytes < 0 {
			s.bytes = 0
		}
		s.files--
		if s.files < 0 {
			s.files = 0
		}
		s.mu.Unlock()
	}
	return os.Rename(src, filepath.Join(dstDir, name))
}

// Stats reports what is waiting: files, bytes, and whether the cap is reached.
func (s *Spool) Stats() (files int, bytes int64, atCap bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files, s.bytes, s.maxBytes > 0 && s.bytes >= s.maxBytes
}

// MarkFull latches the at-capacity state and reports whether this is the first
// call since it last had room, so a caller can log the episode once.
func (s *Spool) MarkFull() (first bool) { return s.full.CompareAndSwap(false, true) }

// Dir is where batches are kept.
func (s *Spool) Dir() string { return s.dir }

// Age returns how long the oldest batch has been waiting, or 0 when empty. A
// growing age means replay is not keeping up and someone needs to look.
func (s *Spool) Age() time.Duration {
	name, _, ok, err := s.oldestName()
	if err != nil || !ok {
		return 0
	}
	fi, serr := os.Stat(filepath.Join(s.dir, name))
	if serr != nil {
		return 0
	}
	return time.Since(fi.ModTime())
}

func (s *Spool) oldestName() (string, []normalizer.FlowRecord, bool, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", nil, false, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), fileExt) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil, false, nil
	}
	sort.Strings(names)
	return names[0], nil, true, nil
}
