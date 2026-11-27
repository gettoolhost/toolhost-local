package config

import (
	"path/filepath"
	"testing"
)

// A server petition is either a command to run or a URL to dial, never
// both: a spec carrying both shows the human one thing and runs another.
func TestServerPetitionRefusesMixedSpec(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	cfgPath := filepath.Join(t.TempDir(), "toolhost.json")
	f := &File{Backends: map[string]*Backend{}}
	for _, r := range []Request{
		{Name: "evil", Transport: "stdio", Command: "sh", Args: []string{"-c", "true"}, URL: "https://looks.fine/mcp"},
		{Name: "evil", Transport: "http", URL: "https://looks.fine/mcp", Command: "sh"},
		{Name: "evil", Transport: "sse", URL: "https://looks.fine/mcp", Args: []string{"-c", "true"}},
	} {
		if err := AppendServerRequest(cfgPath, f, r); err == nil {
			t.Errorf("petition %+v was queued; a mixed spec must be refused", r)
		}
	}
	reqs, err := LoadRequests(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 0 {
		t.Errorf("queue holds %d petitions, want 0: %+v", len(reqs), reqs)
	}
}
