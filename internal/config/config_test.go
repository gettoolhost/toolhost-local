package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestEnabledTier(t *testing.T) {
	f := &File{Approved: []string{"b__a", "b__b", "b__c"}}

	// No allowlist: every approved tool is enabled.
	set, has := f.EnabledSet()
	if has || set != nil {
		t.Fatal("fresh config should have no allowlist")
	}
	if !f.IsEnabled("b__a") {
		t.Fatal("approved tool not enabled without an allowlist")
	}

	// First disable materializes the list as approved ∖ names.
	if _, err := f.Disable("b__a"); err != nil {
		t.Fatal(err)
	}
	if f.IsEnabled("b__a") || !f.IsEnabled("b__b") {
		t.Fatalf("after disable: want a off, b on — got %+v", *f.Enabled)
	}

	// Enable requires approval — never a backdoor around it.
	if _, err := f.Enable("b__unapproved"); err == nil || !strings.Contains(err.Error(), "approve") {
		t.Fatalf("enable unapproved: want approve-first error, got %v", err)
	}
	if _, err := f.Enable("b__a"); err != nil {
		t.Fatal(err)
	}
	if !f.IsEnabled("b__a") {
		t.Fatal("enable did not restore the tool")
	}

	// --only replaces the surface; --all clears the list.
	if _, err := f.EnableOnly("b__b"); err != nil {
		t.Fatal(err)
	}
	if !f.IsEnabled("b__b") || f.IsEnabled("b__a") || f.IsEnabled("b__c") {
		t.Fatalf("after --only b: want only b on — got %+v", *f.Enabled)
	}
	f.EnableAll()
	if f.Enabled != nil || !f.IsEnabled("b__a") {
		t.Fatal("EnableAll should restore the absent-list default")
	}

	// The explicit-empty list must survive a save/load round-trip as
	// "nothing enabled" — not collapse back to "all".
	empty := []string{}
	f.Enabled = &empty
	path := filepath.Join(t.TempDir(), "toolhost.json")
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := loaded.EnabledSet(); !has {
		t.Fatal("explicit empty allowlist collapsed to absent on load")
	}
	if loaded.IsEnabled("b__a") {
		t.Fatal("empty allowlist should disable everything")
	}
}

func TestMode(t *testing.T) {
	// Absent defaults to stateless — the primary serving mode.
	f := &File{}
	f.applyDefaults()
	if f.Mode != ModeStateless {
		t.Fatalf("default mode: want %q, got %q", ModeStateless, f.Mode)
	}
	if err := f.validate(); err != nil {
		t.Fatalf("default mode should validate: %v", err)
	}

	for _, m := range []string{ModeStateless, ModeStateful} {
		f := &File{Mode: m}
		if err := f.validate(); err != nil {
			t.Fatalf("mode %q should validate: %v", m, err)
		}
	}
	for _, m := range []string{"", "sessions", "Stateless"} {
		f := &File{Mode: m}
		if err := f.validate(); err == nil {
			t.Fatalf("mode %q should fail validation", m)
		}
	}

	// Round-trip: an explicit stateful survives save/load.
	path := filepath.Join(t.TempDir(), "toolhost.json")
	f = &File{Token: "t", Mode: ModeStateful}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Mode != ModeStateful {
		t.Fatalf("mode lost on round-trip: got %q", loaded.Mode)
	}
}
