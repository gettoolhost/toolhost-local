package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/core"
)

func recordN(s *Sink, n int) {
	pad := strings.Repeat("x", 400) // ~450B/record — forces real rotations
	for i := 0; i < n; i++ {
		s.Record(core.Event{TS: time.Now(), Kind: core.EventToolCall, Tool: fmt.Sprintf("ev-%03d", i) + pad})
	}
}

// segments returns the live file plus any rotated generations, oldest first.
func segments(t *testing.T, path string) []string {
	t.Helper()
	matches, _ := filepath.Glob(path + "*")
	// "audit.jsonl" sorts before "audit.jsonl.1" lexically — rotate oldest first.
	if len(matches) > 1 && filepath.Ext(matches[0]) == ".jsonl" {
		matches[0], matches[len(matches)-1] = matches[len(matches)-1], matches[0]
	}
	return matches
}

// Rotation is bounded (live + one generation) and loses no writes inside
// the retained window: the surviving records are a contiguous, in-order
// suffix of everything written — never partial lines, never a hole.
func TestRotationRetainsContiguousSuffix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	s, err := Open(path, 4<<10)
	if err != nil {
		t.Fatal(err)
	}
	recordN(s, 20)
	s.Close()

	segs := segments(t, path)
	if len(segs) > 2 {
		t.Fatalf("retention must be bounded, got %d files: %v", len(segs), segs)
	}
	var names []string
	for _, m := range segs {
		f, err := os.Open(m)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var ev core.Event
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				t.Fatalf("partial/interleaved record in %s: %v", m, err)
			}
			names = append(names, ev.Tool)
		}
		f.Close()
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation never happened — 20×450B exceeds the 4KiB cap: %v", err)
	}
	// The survivors are exactly the last len(names) writes, in order.
	if len(names) == 0 || len(names) >= 20 {
		t.Fatalf("retention must be a strict suffix — got %d of 20 records", len(names))
	}
	for i, name := range names {
		want := fmt.Sprintf("ev-%03d", 20-len(names)+i)
		if !strings.HasPrefix(name, want) {
			t.Fatalf("gap in retained evidence: position %d = %.10q, want %q", i, name, want)
		}
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("live file must stay owner-only, got %o", info.Mode().Perm())
	}
}

// No cap → plain append, one file, no rotated segments.
func TestUnlimitedAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	s, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	recordN(s, 5)
	s.Close()
	matches, _ := filepath.Glob(path + "*")
	if len(matches) != 1 {
		t.Fatalf("unlimited audit spawned segments: %v", matches)
	}
}

// The live file honors the cap: rotate before write, so the current
// segment never exceeds maxBytes by more than one record.
func TestLiveFileHonorsCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	s, err := Open(path, 2<<10)
	if err != nil {
		t.Fatal(err)
	}
	recordN(s, 30)
	s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > (2<<10)+512 {
		t.Fatalf("live segment overshot the cap: %d bytes", info.Size())
	}
}
