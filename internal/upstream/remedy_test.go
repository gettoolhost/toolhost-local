package upstream

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/oauth"
)

// A missing grant names the command that grants one. There is no top-level
// `toolhost auth`; the verb is `toolhost mcp auth <name>`.
func TestMissingGrantNamesTheRealCommand(t *testing.T) {
	store, err := oauth.OpenStore(filepath.Join(t.TempDir(), "toolhost_tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := &config.Backend{URL: "https://origin.test/sse", Auth: &config.Auth{Type: "oauth"}}
	for name, opts := range map[string]*Options{"no store": {}, "no grant": {Tokens: store}} {
		_, err := sseClient(context.Background(), "a", b, opts)
		if err == nil || !strings.Contains(err.Error(), "run: toolhost mcp auth a") {
			t.Errorf("%s: got %v, want the remedy `toolhost mcp auth a`", name, err)
		}
	}
}
