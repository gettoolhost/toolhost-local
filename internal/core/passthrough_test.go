package core

import (
	"strings"
	"testing"
)

// A federated gateway's names are trusted for shape, not for wire safety:
// they must pass the same charset and length rules as names we build.
func TestQualifyPassthroughEnforcesWireRules(t *testing.T) {
	up := &stub{ns: "edge", passthrough: true}
	for _, name := range []string{
		"a__has space",                   // outside the wire charset
		"a__" + strings.Repeat("x", 100), // over the 64-character ceiling
	} {
		if q, err := Qualify(up, name); err == nil {
			t.Errorf("Qualify(%q) = %q — a wire-illegal passthrough name was admitted", name, q)
		}
	}
	if q, err := Qualify(up, "fs__read_text_file"); err != nil || q != "fs__read_text_file" {
		t.Errorf("valid passthrough name refused: %q, %v", q, err)
	}
}
