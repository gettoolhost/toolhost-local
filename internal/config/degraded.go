package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A running gateway can serve something other than what its config says:
// the config stopped loading, so the previous one keeps serving, or the
// tools stopped resolving, so none serve. It says so in a record in the
// environment's state dir. `toolhost status` and `toolhost doctor` read the
// record without asking the gateway, which they can't do when the config
// doesn't parse or the gateway speaks stdio. Each record has a lock file
// that its gateway holds for as long as the record is true. A gateway that
// dies loses its lock with its process, so the record it leaves behind is
// ignored and later swept.

// Degraded is one gateway's record.
type Degraded struct {
	// Config is the config's real path, set by MarkDegraded.
	Config string `json:"config"`
	PID    int    `json:"pid"`
	// Listen is the gateway's address, or empty for a stdio gateway.
	Listen string `json:"listen,omitempty"`
	// ConfigError is set while the config on disk doesn't load and the
	// previous one keeps serving.
	ConfigError *Fault `json:"config_error,omitempty"`
	// ResolveError is set while the tools don't resolve and none serve.
	ResolveError *Fault `json:"resolve_error,omitempty"`
}

// Fault is one failure that persists: since when, and the latest error.
type Fault struct {
	Since time.Time `json:"since"`
	Error string    `json:"error"`
}

// DegradedMark is a gateway's hold on its record.
type DegradedMark struct {
	config string // canonical: a relative path means the gateway's folder
	record string
	lock   *os.File
}

func degradedDir(cfgPath string) string { return filepath.Join(StateDir(cfgPath), "degraded") }

// MarkDegraded writes d as this process's record for the config at cfgPath
// and holds the record's lock until Clear.
func MarkDegraded(cfgPath string, d Degraded) (*DegradedMark, error) {
	dir := degradedDir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sweepDegraded(dir)
	base := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	fh, err := os.OpenFile(base+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	// A reader holds the lock only for the moment it takes to test it.
	for try := 0; ; try++ {
		ok, err := tryLock(fh)
		if err != nil {
			fh.Close()
			return nil, fmt.Errorf("lock %s: %w", fh.Name(), err)
		}
		if ok {
			break
		}
		if try == 40 {
			fh.Close()
			return nil, fmt.Errorf("lock %s: held by another process", fh.Name())
		}
		time.Sleep(5 * time.Millisecond)
	}
	m := &DegradedMark{config: canonical(cfgPath), record: base + ".json", lock: fh}
	if err := m.Update(d); err != nil {
		m.Clear()
		return nil, err
	}
	return m, nil
}

// Update replaces the record's contents.
func (m *DegradedMark) Update(d Degraded) error {
	d.Config, d.PID = m.config, os.Getpid()
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return writeAtomic(m.record, raw)
}

// Clear removes the record, then releases and removes its lock. The record
// goes first: a record whose lock is free is never read as true.
func (m *DegradedMark) Clear() {
	if m == nil {
		return
	}
	_ = os.Remove(m.record)
	_ = unlock(m.lock)
	m.lock.Close()
	_ = os.Remove(m.lock.Name())
}

// ReportedDegraded returns the records of running gateways for the config
// at cfgPath. It changes nothing on disk.
func ReportedDegraded(cfgPath string) []Degraded {
	want := canonical(cfgPath)
	var out []Degraded
	for _, rec := range degradedRecords(degradedDir(cfgPath)) {
		if !held(strings.TrimSuffix(rec, ".json") + ".lock") {
			continue
		}
		raw, err := os.ReadFile(rec)
		if err != nil {
			continue
		}
		var d Degraded
		if json.Unmarshal(raw, &d) != nil || canonical(d.Config) != want {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func degradedRecords(dir string) []string {
	recs, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return recs
}

// held reports whether a live process holds the lock at p. Testing it takes
// the lock for a moment when it's free; a missing lock file is free.
func held(p string) bool {
	fh, err := os.Open(p)
	if err != nil {
		return false
	}
	defer fh.Close()
	ok, err := tryLock(fh)
	if err != nil {
		return false
	}
	if ok {
		_ = unlock(fh)
		return false
	}
	return true
}

// sweepDegraded removes records whose gateway is gone. Their lock files
// stay: a lock file removed between a gateway creating it and locking it
// would leave that gateway holding a lock nobody can find.
func sweepDegraded(dir string) {
	for _, rec := range degradedRecords(dir) {
		if !held(strings.TrimSuffix(rec, ".json") + ".lock") {
			_ = os.Remove(rec)
		}
	}
}
