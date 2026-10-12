package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Audit reads the log without a running gateway — a relative audit_log
// anchors to the env's state dir (never the folder holding the config),
// rotated history backfills, kind filters.
func TestAuditCLI(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := filepath.Join(dir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"th_x","audit_log":"audit.jsonl"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := config.StateDir(cfg)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(stateDir, "audit.jsonl")
	writeJSON(t, log, `{"ts":"2026-10-12T01:00:00Z","kind":"tool_call","backend":"fs","tool":"fs__read","ms":5}`+"\n"+
		`{"ts":"2026-10-12T01:00:01Z","kind":"govern","tool":"enable:fs__read"}`+"\n")

	var buf bytes.Buffer
	if err := Audit(cfg, 0, "", &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"tool_call", "fs__read", "5ms", "govern", "enable:fs__read"} {
		if !strings.Contains(out, want) {
			t.Fatalf("audit output missing %q:\n%s", want, out)
		}
	}

	// kind filter narrows to one kind.
	buf.Reset()
	if err := Audit(cfg, 0, "govern", &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "tool_call") || !strings.Contains(buf.String(), "govern") {
		t.Fatalf("kind filter failed:\n%s", buf.String())
	}

	// Empty log is a clean report, not an error.
	buf.Reset()
	writeJSON(t, log, "")
	if err := Audit(cfg, 0, "", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no audit events") {
		t.Fatalf("empty log: %s", buf.String())
	}
}

// ApproveRequested with an empty queue is a clean no-op — it must not
// dial backends or write the config.
func TestApproveRequestedEmpty(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	dir := t.TempDir()
	cfg := filepath.Join(dir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"th_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ApproveRequested(t.Context(), cfg, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no pending agent requests") {
		t.Fatalf("expected empty-queue message, got: %s", buf.String())
	}
}
