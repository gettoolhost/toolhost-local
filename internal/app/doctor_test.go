package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative audit_log anchors to the config's directory — the same
// resolution serve uses — never the process cwd.
func TestDoctorAnchorsAuditDirToConfig(t *testing.T) {
	cfgDir := t.TempDir()
	t.Chdir(t.TempDir()) // cwd holds no config and must stay untouched
	cfg := filepath.Join(cfgDir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"t","audit_log":"logs/audit.jsonl"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	Doctor(context.Background(), cfg, &out)

	anchored := filepath.Join(cfgDir, "logs")
	if !strings.Contains(out.String(), anchored) {
		t.Fatalf("doctor reported a dir other than the config-anchored %s:\n%s", anchored, out.String())
	}
}

// Doctor diagnoses — it must not create the audit directory it probes.
func TestDoctorDoesNotCreateAuditDir(t *testing.T) {
	cfgDir := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	cfg := filepath.Join(cfgDir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"t","audit_log":"logs/audit.jsonl"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	Doctor(context.Background(), cfg, &out)

	for _, p := range []string{filepath.Join(cfgDir, "logs"), filepath.Join(cwd, "logs")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("doctor created %s — a diagnostic must not mutate", p)
		}
	}
}

// With the dir present, the writability probe reports the anchored path.
func TestDoctorProbesAnchoredAuditDir(t *testing.T) {
	cfgDir := t.TempDir()
	t.Chdir(t.TempDir())
	if err := os.Mkdir(filepath.Join(cfgDir, "logs"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(cfgDir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"t","audit_log":"logs/audit.jsonl"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	Doctor(context.Background(), cfg, &out)

	want := "✓ audit log dir " + filepath.Join(cfgDir, "logs") + " writable"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("want %q in output:\n%s", want, out.String())
	}
}
