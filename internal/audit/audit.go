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

	"github.com/gettoolhost/toolhost-local/internal/core"
)

// Sink appends core.Events as JSONL.
type Sink struct {
	mu sync.Mutex
	f  *os.File
}

func Open(path string) (*Sink, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	return &Sink{f: f}, nil
}

func (s *Sink) Record(ev core.Event) {
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.f.Write(append(raw, '\n'))
}

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}

var _ core.AuditSink = (*Sink)(nil)

// Discard is the audit sink for callers that intentionally keep no evidence
// (e.g. `discover` — a read-only preview, not a governed call).
type Discard struct{}

func (Discard) Record(core.Event) {}
