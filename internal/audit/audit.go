// Package audit is the driven adapter for evidence: one JSON object per
// governed action, appended to a JSONL file. Boring on purpose — the
// evidence contract is "every call leaves a record", and a flat file every
// UNIX tool can read is the honest minimum.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/core"
)

// rotateRetryAfter gates rotation attempts — a failing rename must not
// turn every write into a rename attempt.
const rotateRetryAfter = time.Minute

// Sink appends core.Events as JSONL. Past maxBytes the file rotates to
// <path>.1 — one generation kept. Evidence is append-mostly; recent
// history is what introspection needs, and unbounded growth is the failure
// that actually hurts. maxBytes <= 0 disables rotation.
type Sink struct {
	mu       sync.Mutex
	f        *os.File
	path     string
	maxBytes int64
	size     int64
	// rotateErrAt backs off rotation after a failure — otherwise a stuck
	// rename would be retried on every single record.
	rotateErrAt time.Time
}

func Open(path string, maxBytes int64) (*Sink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	s := &Sink{f: f, path: path, maxBytes: maxBytes}
	if fi, err := f.Stat(); err == nil {
		s.size = fi.Size()
	}
	return s, nil
}

func (s *Sink) Record(ev core.Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	raw = append(raw, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		// A previous rotation left us unfiled — try to reopen rather than
		// silently dropping evidence forever.
		f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		s.f = f
		s.syncSize()
	}
	if s.maxBytes > 0 && s.size+int64(len(raw)) > s.maxBytes &&
		time.Since(s.rotateErrAt) > rotateRetryAfter {
		s.rotate()
	}
	n, _ := s.f.Write(raw)
	s.size += int64(n)
}

// syncSize re-reads the file's size so a fallback reopen doesn't restart
// the cap accounting from stale state.
func (s *Sink) syncSize() {
	if fi, err := s.f.Stat(); err == nil {
		s.size = fi.Size()
	}
}

// rotate swaps the live log for a fresh file, keeping one generation at
// <path>.1. Rotation failure is not a reason to lose evidence — the sink
// keeps appending to whatever file it can open, and rotateErrAt throttles
// the next attempt so a stuck rename isn't retried per-record.
func (s *Sink) rotate() {
	_ = s.f.Close()
	if err := os.Rename(s.path, s.path+".1"); err != nil {
		// rename failed — reopen the original and keep appending
		s.rotateErrAt = time.Now()
		if f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			s.f = f
			s.syncSize()
		} else {
			s.f = nil
		}
		return
	}
	if f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err == nil {
		s.f = f
		s.size = 0
		s.rotateErrAt = time.Time{}
	} else {
		s.f = nil
		s.rotateErrAt = time.Now()
	}
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	return s.f.Close()
}

var _ core.AuditSink = (*Sink)(nil)

// Discard is the audit sink for callers that intentionally keep no evidence
// (e.g. `discover` — a read-only preview, not a governed call).
type Discard struct{}

func (Discard) Record(core.Event) {}
