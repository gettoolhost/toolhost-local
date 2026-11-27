package config

import "testing"

// With no enabled list every approved tool is already on — but an
// unapproved or malformed name must still be refused, not silently
// accepted as a no-op the agent can't tell from success.
func TestEnableRefusesUnapprovedWithoutAllowlist(t *testing.T) {
	f := &File{Approved: []string{"a__x"}}
	for _, name := range []string{"a__y", "not-qualified"} {
		if got, err := f.Enable(name); err == nil {
			t.Errorf("Enable(%q) with no enabled list = %v, nil — want a refusal", name, got)
		}
	}
	got, err := f.Enable("a__x")
	if err != nil || len(got) != 0 || f.Enabled != nil {
		t.Fatalf("Enable(approved) with no list: want no change and no error, got %v, %v, list %v", got, err, f.Enabled)
	}
}
