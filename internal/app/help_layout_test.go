package app

import (
	"bytes"
	"strings"
	"testing"
)

// Every group in `toolhost help` has a title; a blank separates groups.
// A line of only spaces is a header with no name.
func TestHelpHasNoUntitledGroup(t *testing.T) {
	var buf bytes.Buffer
	Help(nil, &buf)
	for i, line := range strings.Split(buf.String(), "\n") {
		if line != "" && strings.TrimSpace(line) == "" {
			t.Errorf("help line %d is only spaces (%q): an untitled group header", i+1, line)
		}
	}
	if !strings.Contains(buf.String(), "    version ") {
		t.Errorf("help no longer lists version")
	}
}
