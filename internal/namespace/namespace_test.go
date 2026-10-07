package namespace

import (
	"errors"
	"strings"
	"testing"
)

func TestJoin(t *testing.T) {
	q, err := Join("fs", "read_file")
	if err != nil || q != "fs__read_file" {
		t.Fatalf("want fs__read_file, got %q err=%v", q, err)
	}

	cases := []struct {
		ns, name string
		wantErr  error
	}{
		{"fs", "", nil},                                 // empty name — generic error
		{"fs", "has__sep", nil},                         // name contains separator
		{"fs", "has.dot", ErrNameCharset},               // wire-illegal char
		{"fs", "has space", ErrNameCharset},             // wire-illegal char
		{"fs", "café", ErrNameCharset},                  // non-ASCII
		{"fs", strings.Repeat("x", 61), ErrNameTooLong}, // namespaced > 64
		{"bad ns", "ok", nil},                           // invalid namespace
	}
	for _, c := range cases {
		_, err := Join(c.ns, c.name)
		if err == nil {
			t.Fatalf("Join(%q, %q): want error, got nil", c.ns, c.name)
		}
		if c.wantErr != nil && !errors.Is(err, c.wantErr) {
			t.Fatalf("Join(%q, %q): want errors.Is %v, got %v", c.ns, c.name, c.wantErr, err)
		}
	}

	// Exactly at the ceiling passes.
	ns := strings.Repeat("a", 3)
	name := strings.Repeat("b", MaxNamespacedNameLength-3-2)
	if _, err := Join(ns, name); err != nil {
		t.Fatalf("64-char name should pass: %v", err)
	}
}

func TestSplit(t *testing.T) {
	ns, name, err := Split("github__search_repos")
	if err != nil || ns != "github" || name != "search_repos" {
		t.Fatalf("want github/search_repos, got %q/%q err=%v", ns, name, err)
	}
	for _, bad := range []string{"noseparator", "a__b__c", "__tool", "ns__"} {
		if _, _, err := Split(bad); err == nil {
			t.Fatalf("Split(%q): want error, got nil", bad)
		}
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, ok := range []string{"fs", "github", "my-backend", "openai_docs", "b2"} {
		if err := ValidateNamespace(ok); err != nil {
			t.Fatalf("ValidateNamespace(%q): want ok, got %v", ok, err)
		}
	}
	cases := map[string]string{
		"":         "empty",
		"has__sep": "contains separator",
		"trail_":   "trailing underscore collides: a_+b vs a+_b",
		"Upper":    "uppercase",
		"dot.name": "dot",
		"space me": "space",
	}
	for ns, why := range cases {
		if err := ValidateNamespace(ns); err == nil {
			t.Fatalf("ValidateNamespace(%q): want error (%s), got nil", ns, why)
		}
	}

	// The byte-collision the trailing-underscore rule exists for:
	// Join("a_", "b") and Join("a", "_b") both spell "a___b".
	if _, err := Join("a_", "b"); err == nil {
		t.Fatal("a_+b must fail — collides with a+_b")
	}
}
