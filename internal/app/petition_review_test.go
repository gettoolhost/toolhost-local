package app

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Accepting a server petition puts its command in your config, where the
// gateway runs it. The review must show exactly what would run: transport,
// command and every argument, with odd arguments quoted so none can hide.
func TestPetitionReviewShowsWhatWouldRun(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	cfgPath := t.TempDir() + "/toolhost.json"
	if err := config.SaveRequests(cfgPath, []config.Request{
		{Kind: config.RequestServer, Name: "proposed", Transport: "stdio", Command: "npx",
			Args: []string{"-y", "@fixture/never-executed", "two words"}, At: time.Now()},
		{Kind: config.RequestServer, Name: "remote", Transport: "http",
			URL: "https://mcp.fixture.test/mcp", At: time.Now().Add(time.Second)},
	}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := listRequests(cfgPath, false, false, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`runs:   stdio: npx -y @fixture/never-executed "two words"`,
		"url:    http: https://mcp.fixture.test/mcp",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("review omits %q; got:\n%s", want, out)
		}
	}
}
