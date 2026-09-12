// Package spool implements the ClickHouse writer's bounded batch write-ahead
// log, so an outage or crash costs time instead of evidence.
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
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
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
	fileExt   = ".spool"
	tmpExt    = ".tmp"
	activeExt = ".active"
	idFile    = ".wal-id"
)

var walHeader = [...]byte{'N', 'A', 'T', 'W', 'A', 'L', 1, 0}

// Spool is a bounded, crash-safe directory of batches awaiting insert.
//
// Ordering is by filename, which embeds a monotonic sequence, so replay is
// roughly first-in-first-out. Exact ordering does not matter — every record
// carries its own timestamps and the table is not append-ordered — but oldest
// first means the longest-waiting evidence lands soonest.
type Spool struct {
	dir      string
	maxBytes int64
	id       string

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
	id, err := loadOrCreateID(dir)
	if err != nil {
		return nil, fmt.Errorf("spool identity: %w", err)
	}
	s := &Spool{dir: dir, maxBytes: maxBytes, id: id}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("spool read %s: %w", dir, err)
	}
	adopted := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if name == "quarantine" {
				qEntries, qerr := os.ReadDir(filepath.Join(dir, name))
				if qerr != nil {
					return nil, fmt.Errorf("spool read quarantine: %w", qerr)
				}
				for _, qe := range qEntries {
					if qe.IsDir() || !strings.HasSuffix(qe.Name(), fileExt) {
						continue
					}
					fi, ferr := qe.Info()
					if ferr != nil {
						return nil, fmt.Errorf("spool stat quarantine %s: %w", qe.Name(), ferr)
					}
					s.bytes += fi.Size()
					if n := seqOf(qe.Name()); n > s.seq {
						s.seq = n
					}
				}
			}
			continue
		}
		// A .tmp file is a write that did not finish; its batch was never
		// acknowledged as spooled, so it is incomplete and must not be replayed.
		if strings.HasSuffix(name, tmpExt) {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		// An active file was fsynced before its owner attempted ClickHouse. If
		// it is still here during startup, the process died before it could
		// either commit or release the batch. Make it replayable. This can
		// produce a duplicate when ClickHouse committed but its acknowledgement
		// did not reach us; delivery is intentionally at-least-once.
		if strings.HasSuffix(name, fileExt+activeExt) {
			ready := strings.TrimSuffix(name, activeExt)
			if _, err := os.Stat(filepath.Join(dir, ready)); err == nil {
				return nil, fmt.Errorf("spool contains both active and ready copies of %s", ready)
			} else if !os.IsNotExist(err) {
				return nil, fmt.Errorf("spool inspect adopted target %s: %w", ready, err)
			}
			if err := os.Rename(filepath.Join(dir, name), filepath.Join(dir, ready)); err != nil {
				return nil, fmt.Errorf("spool adopt %s: %w", name, err)
			}
			name = ready
			adopted = true
		}
		if !strings.HasSuffix(name, fileExt) {
			continue
		}
		fi, ferr := os.Stat(filepath.Join(dir, name))
		if ferr != nil {
			continue
		}
		s.bytes += fi.Size()
		s.files++
		if n := seqOf(name); n > s.seq {
			s.seq = n
		}
	}
	if adopted {
		if err := syncDir(dir); err != nil {
			return nil, fmt.Errorf("spool sync adopted entries: %w", err)
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

// Begin writes a batch durably before its first ClickHouse attempt. The returned
// name is a stable handle; the on-disk file remains hidden from the replay loop
// until Ready is called. On process restart New adopts every active file for
// replay, which closes the crash window between database send and acknowledgement.
//
// It returns ErrFull when the cap is reached: filling the filesystem would take
// the whole collector down and cost every exporter rather than one batch.
func (s *Spool) Begin(batch []normalizer.FlowRecord) (string, error) {
	if len(batch) == 0 {
		return "", nil
	}
	s.mu.Lock()
	if s.maxBytes > 0 && s.bytes >= s.maxBytes {
		s.mu.Unlock()
		return "", ErrFull
	}
	s.seq++
	name := fmt.Sprintf("%020d%s", s.seq, fileExt)
	s.mu.Unlock()

	active := filepath.Join(s.dir, name+activeExt)
	tmp := filepath.Join(s.dir, name+tmpExt)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", fmt.Errorf("spool create: %w", err)
	}
	if err := encodeBatch(f, batch); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool encode: %w", err)
	}
	// fsync before rename: the fault this guards against is an unclean shutdown,
	// and a batch that only reached the page cache would not survive one.
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool sync: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool stat: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool close: %w", err)
	}
	if err := os.Rename(tmp, active); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("spool rename active: %w", err)
	}
	if err := syncDir(s.dir); err != nil {
		_ = os.Remove(active)
		return "", fmt.Errorf("spool sync directory: %w", err)
	}
	s.mu.Lock()
	s.bytes += fi.Size()
	s.files++
	s.mu.Unlock()
	s.full.Store(false)
	return name, nil
}

// Ready releases a write-ahead batch to the replay loop after its live
// ClickHouse attempts fail. The rename is atomic and its directory entry is
// fsynced, so a power loss cannot silently turn a released batch back into a
// transient file.
func (s *Spool) Ready(name string) error {
	if name == "" {
		return nil
	}
	ready := filepath.Join(s.dir, name)
	active := ready + activeExt
	if _, err := os.Stat(ready); err == nil {
		return nil // idempotent release
	}
	if err := os.Rename(active, ready); err != nil {
		return fmt.Errorf("spool release %s: %w", name, err)
	}
	if err := syncDir(s.dir); err != nil {
		return fmt.Errorf("spool sync released entry %s: %w", name, err)
	}
	return nil
}

// Save is retained for callers and legacy tests that want to place an already
// failed batch directly onto the replay queue.
func (s *Spool) Save(batch []normalizer.FlowRecord) error {
	name, err := s.Begin(batch)
	if err != nil || name == "" {
		return err
	}
	return s.Ready(name)
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
	if derr := decodeBatch(f, &batch); derr != nil && derr != io.EOF {
		return name, nil, false, fmt.Errorf("spool decode %s: %w", name, derr)
	}
	return name, batch, true, nil
}

// Done removes a batch that was successfully inserted.
func (s *Spool) Done(name string) error {
	p := filepath.Join(s.dir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		p += activeExt
	}
	fi, err := os.Stat(p)
	if err := os.Remove(p); err != nil {
		return err
	}
	if err == nil {
		s.removeStats(fi.Size())
	}
	return syncDir(s.dir)
}

// Quarantine moves a batch aside after repeated failures so one undecodable or
// permanently rejected file cannot block every batch behind it forever. It stays
// on disk: this is evidence, and deleting it to unblock a queue would repeat the
// mistake this package exists to correct.
func (s *Spool) Quarantine(name string) error {
	src := filepath.Join(s.dir, name)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		src += activeExt
	}
	dstDir := filepath.Join(s.dir, "quarantine")
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, strings.TrimSuffix(filepath.Base(src), activeExt))
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("quarantine target already exists: %s", dst)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	s.quarantineStats()
	if err := syncDir(dstDir); err != nil {
		return err
	}
	return syncDir(s.dir)
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

// DedupToken namespaces a batch handle with a persistent random spool identity.
// Filenames alone collide across collectors because every host starts at sequence
// one; using such a filename as a ClickHouse token would discard valid fleet data.
func (s *Spool) DedupToken(name string) string {
	if name == "" {
		return ""
	}
	return s.id + ":" + name
}

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

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func loadOrCreateID(dir string) (string, error) {
	p := filepath.Join(dir, idFile)
	if b, err := os.ReadFile(p); err == nil {
		id := strings.TrimSpace(string(b))
		if raw, decErr := hex.DecodeString(id); decErr == nil && len(raw) == 16 {
			return id, nil
		}
		return "", fmt.Errorf("%s is not a 128-bit hex identity", p)
	} else if !os.IsNotExist(err) {
		return "", err
	}

	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw[:])
	tmp := p + tmpExt
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(id + "\n"); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return id, nil
}

func encodeBatch(w io.Writer, batch []normalizer.FlowRecord) error {
	if _, err := w.Write(walHeader[:]); err != nil {
		return err
	}
	h := sha256.New()
	if err := gob.NewEncoder(io.MultiWriter(w, h)).Encode(batch); err != nil {
		return err
	}
	_, err := w.Write(h.Sum(nil))
	return err
}

func decodeBatch(f *os.File, batch *[]normalizer.FlowRecord) error {
	var header [len(walHeader)]byte
	if _, err := io.ReadFull(f, header[:]); err != nil || !bytes.Equal(header[:], walHeader[:]) {
		// Files created before the WAL envelope are plain gob. They remain
		// replayable so an upgrade never strands outage evidence.
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			return seekErr
		}
		return gob.NewDecoder(f).Decode(batch)
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	payloadBytes := fi.Size() - int64(len(walHeader)) - sha256.Size
	if payloadBytes < 1 {
		return fmt.Errorf("invalid WAL envelope length %d", fi.Size())
	}
	h := sha256.New()
	limited := &io.LimitedReader{R: f, N: payloadBytes}
	tee := io.TeeReader(limited, h)
	if err := gob.NewDecoder(tee).Decode(batch); err != nil {
		return err
	}
	// gob may stop after the decoded value. Hash any payload bytes it did not
	// request before reading the fixed-size checksum trailer.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return err
	}
	want := make([]byte, sha256.Size)
	if _, err := io.ReadFull(f, want); err != nil {
		return err
	}
	if got := h.Sum(nil); !bytes.Equal(got, want) {
		return fmt.Errorf("WAL checksum mismatch")
	}
	return nil
}

func (s *Spool) removeStats(size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes -= size
	if s.bytes < 0 {
		s.bytes = 0
	}
	s.files--
	if s.files < 0 {
		s.files = 0
	}
}

func (s *Spool) quarantineStats() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files--
	if s.files < 0 {
		s.files = 0
	}
}
