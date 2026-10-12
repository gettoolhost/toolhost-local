package app_test

// The three init paths: global (private, in home), --project (private env
// keyed by this folder, in home), --shared (committable ./toolhost.json
// with an env: reference — never a literal credential).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/app"
	"github.com/gettoolhost/toolhost-local/internal/config"
)

func TestInitWritesTokenAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolhost.json")
	var buf bytes.Buffer
	if err := app.Init(path, &buf); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.Token, "th_") {
		t.Fatalf("want a generated th_ token, got %q", f.Token)
	}
	if err := app.Init(path, &buf); err == nil {
		t.Fatal("init must refuse to overwrite an existing config")
	}
}

// init --project keys the private env under the user's home — the folder
// itself gains no files, so nothing can be committed or leaked.
func TestInitProjectKeysPrivateEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TOOLHOST_HOME", home)
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	if err := app.InitProject(&buf); err != nil {
		t.Fatal(err)
	}
	want := config.ProjectConfigPath(dir)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("private env not written at %s: %v", want, err)
	}
	if !strings.HasPrefix(want, filepath.Join(home, "projects")) {
		t.Fatalf("private env escaped the projects dir: %s", want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".toolhost")); !os.IsNotExist(err) {
		t.Fatal("init --project must not create ./.toolhost/")
	}
	if _, err := os.Stat(filepath.Join(dir, "toolhost.json")); !os.IsNotExist(err) {
		t.Fatal("init --project must not create ./toolhost.json")
	}
	// And it resolves: this folder's env wins over shared/global.
	if got := config.DefaultPath(""); got != want {
		t.Fatalf("private env should resolve first, got %q", got)
	}
	// Idempotent refusal — a second init does not clobber the env's token.
	if err := app.InitProject(&buf); err == nil {
		t.Fatal("init --project must refuse an existing env")
	}
}

// init --shared writes ./.toolhost/toolhost.json holding an env:
// reference — the committable shape carries no literal credential, and
// the hidden namespaced path reads as tooling, not project content.
func TestInitSharedWritesEnvRef(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)

	var buf bytes.Buffer
	if err := app.InitShared(&buf); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".toolhost", "toolhost.json"))
	if err != nil {
		t.Fatal(err)
	}
	// And it resolves — the shared file is the env for this folder.
	if got := config.DefaultPath(""); got != config.SharedConfigPath {
		t.Fatalf("shared env should resolve, got %q", got)
	}
	if strings.Contains(string(raw), "th_") {
		t.Fatalf("shared config must not carry a literal token:\n%s", raw)
	}
	if !strings.Contains(string(raw), "env:TOOLHOST_TOKEN") {
		t.Fatalf("shared config must reference the env: %s", raw)
	}
}

// toolhost serve --stdio needs no bearer — the pipe is the boundary. A
// shared config with an env: ref must serve stdio even with the var unset.
// (Covered by Serve's stdio path not consulting ResolvedToken — asserted
// structurally in the e2e suite; here we check the config loads lazily.)
func TestSharedConfigLoadsWithoutToken(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	var buf bytes.Buffer
	if err := app.InitShared(&buf); err != nil {
		t.Fatal(err)
	}
	f, err := config.Load(filepath.Join(dir, ".toolhost", "toolhost.json"))
	if err != nil {
		t.Fatalf("env: ref must not break load: %v", err)
	}
	if _, err := f.ResolvedToken(); err == nil {
		t.Fatal("unresolved token should fail closed at the boundary")
	}
}
