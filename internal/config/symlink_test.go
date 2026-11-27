package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// One config reached by two spellings — a symlinked folder, or macOS's
// /tmp and /private/tmp — is still one file: same lock, same stale check.
func TestStaleCheckHoldsAcrossSymlinkedPaths(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "lnk")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realPath := filepath.Join(real, "toolhost.json")
	linkPath := filepath.Join(link, "toolhost.json")
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{"a__x"}}
	if err := seed.Save(realPath); err != nil {
		t.Fatal(err)
	}

	// Read through one spelling, written back through the other.
	old, err := Load(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(linkPath, func(f *File) error { f.Revoke("a__x"); return nil }); err != nil {
		t.Fatal(err)
	}
	old.Approve(map[string]string{"b__y": ""})
	if err := old.Save(linkPath); !errors.Is(err, ErrStale) {
		t.Errorf("stale save through the symlinked path: got %v, want ErrStale", err)
	}
	final, err := Load(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if final.IsApproved("a__x") {
		t.Errorf("revoked a__x is approved again")
	}
	if a, b := configLock(realPath), configLock(linkPath); a != b {
		t.Errorf("two spellings of one config take different locks:\n %s\n %s", a, b)
	}
}
