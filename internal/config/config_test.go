package config

import (
	"os"
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

func TestCallTimeoutValidation(t *testing.T) {
	// Absent means "use the default" — valid at both levels.
	for _, ct := range []string{"", "60s", "2m", "150ms"} {
		f := &File{Mode: ModeStateless, CallTimeout: ct}
		if err := f.validate(); err != nil {
			t.Fatalf("call_timeout %q should validate: %v", ct, err)
		}
	}
	for _, bad := range []string{"abc", "-5s", "0s", "60"} {
		f := &File{Mode: ModeStateless, CallTimeout: bad}
		if err := f.validate(); err == nil {
			t.Fatalf("call_timeout %q should fail validation", bad)
		}
	}

	// Per-backend: same rules — absent is inherit, garbage fails closed.
	good := &File{Mode: ModeStateless, Backends: map[string]*Backend{
		"b": {Transport: "http", URL: "http://x", CallTimeout: "30s"},
	}}
	if err := good.validate(); err != nil {
		t.Fatalf("backend call_timeout should validate: %v", err)
	}
	bad := &File{Mode: ModeStateless, Backends: map[string]*Backend{
		"b": {Transport: "http", URL: "http://x", CallTimeout: "soon"},
	}}
	if err := bad.validate(); err == nil || !strings.Contains(err.Error(), "call_timeout") {
		t.Fatalf("backend call_timeout %q should fail, got %v", "soon", err)
	}

	// Absent → the documented default after load.
	path := filepath.Join(t.TempDir(), "toolhost.json")
	f := &File{Token: "t"}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CallTimeout != DefaultCallTimeout {
		t.Fatalf("default call_timeout: want %q, got %q", DefaultCallTimeout, loaded.CallTimeout)
	}
}

func TestEnvRefs(t *testing.T) {
	t.Setenv("TH_TEST_TOKEN", "resolved-secret")
	t.Setenv("TH_TEST_KEY", "up-key")

	f := &File{Mode: ModeStateless, Token: "env:TH_TEST_TOKEN", Backends: map[string]*Backend{
		"b": {Transport: "http", URL: "http://x",
			Headers: map[string]string{"X-Key": "env:TH_TEST_KEY"},
			Auth:    &Auth{Type: "bearer", Token: "env:TH_TEST_TOKEN"}},
	}}
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}

	// Resolution happens at the boundary — File itself keeps references.
	tok, err := f.ResolvedToken()
	if err != nil || tok != "resolved-secret" {
		t.Fatalf("ResolvedToken: %q %v", tok, err)
	}
	rb, err := f.Backends["b"].Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if rb.Headers["X-Key"] != "up-key" || rb.Auth.Token != "resolved-secret" {
		t.Fatalf("Resolved: %+v", rb)
	}
	// The original must be untouched — the reference, not the secret.
	if f.Backends["b"].Auth.Token != "env:TH_TEST_TOKEN" {
		t.Fatalf("Resolved mutated the receiver: %q", f.Backends["b"].Auth.Token)
	}

	// Unset env → fail closed at load.
	bad := &File{Mode: ModeStateless, Token: "env:TH_TEST_MISSING"}
	if err := bad.validate(); err == nil {
		t.Fatal("unset env var should fail validation")
	}

	// Save round-trips the reference — a resolved secret can never land
	// on disk via governance writes.
	path := filepath.Join(t.TempDir(), "toolhost.json")
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "resolved-secret") || strings.Contains(string(raw), "up-key") {
		t.Fatalf("Save persisted a resolved secret:\n%s", raw)
	}
	if !strings.Contains(string(raw), "env:TH_TEST_TOKEN") {
		t.Fatalf("Save lost the env: reference:\n%s", raw)
	}
}

func TestRequestTools(t *testing.T) {
	f := &File{Approved: []string{"b__ok"}}

	// Qualified + unapproved → queued with the reason.
	added, err := f.RequestTools("need it", "b__want")
	if err != nil || len(added) != 1 {
		t.Fatalf("request: %v %v", added, err)
	}
	if !f.RequestedSet()["b__want"] || f.Requested[0].Reason != "need it" {
		t.Fatalf("request not recorded: %+v", f.Requested)
	}

	// Deduped — a second ask adds nothing.
	added, err = f.RequestTools("again", "b__want")
	if err != nil || len(added) != 0 || len(f.Requested) != 1 {
		t.Fatalf("dedup: %+v %v", f.Requested, err)
	}

	// Bad names and already-approved tools are refused.
	if _, err := f.RequestTools("", "badname"); err == nil {
		t.Fatal("unqualified name accepted")
	}
	if _, err := f.RequestTools("", "b__ok"); err == nil || !strings.Contains(err.Error(), "already approved") {
		t.Fatalf("approved name accepted: %v", err)
	}

	// A request never widens the gate on its own.
	if f.IsApproved("b__want") || f.IsEnabled("b__want") {
		t.Fatal("a request must not approve or enable")
	}

	// Approve consumes the request — the ask is answered.
	if _, err := f.Approve("b__want"); err != nil {
		t.Fatal(err)
	}
	if len(f.Requested) != 0 || !f.IsApproved("b__want") {
		t.Fatalf("approve should consume the request: %+v", f.Requested)
	}
}

func TestStdioEnvConfig(t *testing.T) {
	// env knobs are stdio-only — on http they would be silently dead
	// config, and ambiguity denies.
	for _, b := range []*Backend{
		{Transport: "http", URL: "http://x", Env: map[string]string{"K": "v"}},
		{Transport: "http", URL: "http://x", EnvAllowlist: []string{"K"}},
		{Transport: "http", URL: "http://x", EnvInherit: true},
	} {
		f := &File{Mode: ModeStateless, Backends: map[string]*Backend{"b": b}}
		if err := f.validate(); err == nil || !strings.Contains(err.Error(), "stdio") {
			t.Fatalf("env on non-stdio should fail, got %v", err)
		}
	}

	// env_inherit already passes everything — an allowlist on top is a
	// contradictory config, not a narrower one.
	f := &File{Mode: ModeStateless, Backends: map[string]*Backend{
		"b": {Transport: "stdio", Command: "x", EnvInherit: true, EnvAllowlist: []string{"K"}},
	}}
	if err := f.validate(); err == nil {
		t.Fatal("env_inherit + env_allowlist should fail validation")
	}

	// Each knob alone validates.
	for _, b := range []*Backend{
		{Transport: "stdio", Command: "x", Env: map[string]string{"K": "v"}},
		{Transport: "stdio", Command: "x", EnvAllowlist: []string{"K"}},
		{Transport: "stdio", Command: "x", EnvInherit: true},
	} {
		f := &File{Mode: ModeStateless, Backends: map[string]*Backend{"b": b}}
		if err := f.validate(); err != nil {
			t.Fatalf("stdio env config should validate: %v", err)
		}
	}
}

func TestEnvValuesResolveRefs(t *testing.T) {
	t.Setenv("TH_TEST_KEY", "up-key")

	b := &Backend{Transport: "stdio", Command: "x",
		Env: map[string]string{"API_KEY": "env:TH_TEST_KEY", "PLAIN": "literal"}}
	rb, err := b.Resolved()
	if err != nil {
		t.Fatal(err)
	}
	if rb.Env["API_KEY"] != "up-key" || rb.Env["PLAIN"] != "literal" {
		t.Fatalf("env refs unresolved: %+v", rb.Env)
	}
	// Receiver keeps the reference — the resolved secret never lands in
	// the in-memory shape Save can write.
	if b.Env["API_KEY"] != "env:TH_TEST_KEY" {
		t.Fatalf("Resolved mutated the receiver: %q", b.Env["API_KEY"])
	}

	// A missing reference fails closed at validation.
	bad := &File{Mode: ModeStateless, Backends: map[string]*Backend{
		"b": {Transport: "stdio", Command: "x", Env: map[string]string{"K": "env:TH_TEST_MISSING"}},
	}}
	if err := bad.validate(); err == nil {
		t.Fatal("unset env: reference in env value should fail validation")
	}
}
