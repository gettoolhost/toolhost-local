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

// A gateway whose config stops loading keeps serving the last good one. It
// says so in a record in the environment's state dir, which `toolhost
// status` and `toolhost doctor` can read even when the config itself
// doesn't parse. Each record has a lock file that its gateway holds for as
// long as the record is true. A gateway that dies loses its lock with its
// process, so the record it leaves behind is ignored and later swept.

// Invalid is one gateway's record: since when the config on disk has failed to
// load, the latest error, and which gateway it is.
type Invalid struct {
	// Config is the config's real path, set by MarkInvalid.
	Config string    `json:"config"`
	Since  time.Time `json:"since"`
	Err    string    `json:"error"`
	PID    int       `json:"pid"`
	// Listen is the gateway's address, or empty for a stdio gateway.
	Listen string `json:"listen,omitempty"`
}

// InvalidMark is a gateway's hold on its record.
type InvalidMark struct {
	config string // canonical: a relative path means the gateway's folder
	record string
	lock   *os.File
}

func invalidDir(cfgPath string) string { return filepath.Join(StateDir(cfgPath), "invalid") }

// MarkInvalid writes s as this process's record for the config at cfgPath and
// holds the record's lock until Clear.
func MarkInvalid(cfgPath string, s Invalid) (*InvalidMark, error) {
	dir := invalidDir(cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	sweepInvalid(dir)
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
	m := &InvalidMark{config: canonical(cfgPath), record: base + ".json", lock: fh}
	if err := m.Update(s); err != nil {
		m.Clear()
		return nil, err
	}
	return m, nil
}

// Update replaces the record's contents.
func (m *InvalidMark) Update(s Invalid) error {
	s.Config, s.PID = m.config, os.Getpid()
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeAtomic(m.record, raw)
}

// Clear removes the record, then releases and removes its lock. The record
// goes first: a record whose lock is free is never read as true.
func (m *InvalidMark) Clear() {
	if m == nil {
		return
	}
	_ = os.Remove(m.record)
	_ = unlock(m.lock)
	m.lock.Close()
	_ = os.Remove(m.lock.Name())
}

// ReportedInvalid returns the records of running gateways that can't load the
// config at cfgPath and are serving their previous one. It changes nothing
// on disk.
func ReportedInvalid(cfgPath string) []Invalid {
	want := canonical(cfgPath)
	var out []Invalid
	for _, rec := range invalidRecords(invalidDir(cfgPath)) {
		if !held(strings.TrimSuffix(rec, ".json") + ".lock") {
			continue
		}
		raw, err := os.ReadFile(rec)
		if err != nil {
			continue
		}
		var s Invalid
		if json.Unmarshal(raw, &s) != nil || canonical(s.Config) != want {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func invalidRecords(dir string) []string {
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

// sweepInvalid removes records whose gateway is gone. Their lock files stay:
// a lock file removed between a gateway creating it and locking it would
// leave that gateway holding a lock nobody can find.
func sweepInvalid(dir string) {
	for _, rec := range invalidRecords(dir) {
		if !held(strings.TrimSuffix(rec, ".json") + ".lock") {
			_ = os.Remove(rec)
		}
	}
}
