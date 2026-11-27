package oauth

import (
	"context"
	"strings"
	"testing"
)

// The non-interactive fetcher fails with the command that grants access.
func TestFailFetcherNamesTheRealCommand(t *testing.T) {
	_, err := failFetcher("a")(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "run: toolhost mcp auth a") {
		t.Errorf("got %v, want the remedy `toolhost mcp auth a`", err)
	}
}
