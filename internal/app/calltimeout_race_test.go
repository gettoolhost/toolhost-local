package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
)

// A hot reload sets the shared call_timeout while calls are in flight on
// sessions the reload kept. Run under -race: the write and the reads must
// be synchronised.
func TestHotReloadedCallTimeoutIsRaceFree(t *testing.T) {
	ctx := context.Background()
	up := echoServer(t)
	live, cfg, cfgPath := testLive(t)
	cfg.Backends = map[string]*config.Backend{"up": {Transport: "http", URL: up.URL}}
	cfg.Approved = []string{"up__echo"}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// Same config as on disk, so reload keeps the session instead of redialling.
	live.cfgs = map[string]*config.Backend{"up": loaded.Backends["up"]}
	b, err := upstream.Connect(ctx, "up", loaded.Backends["up"], live.opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	live.ups = map[string]core.Upstream{"up": b}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			live.reload()
		}
	}()
	args := json.RawMessage(`{"message":"hi"}`)
	for {
		select {
		case <-done:
			if _, err := b.Call(ctx, "echo", args); err != nil {
				t.Fatalf("call after reloads: %v", err)
			}
			return
		default:
			_, _ = b.Call(ctx, "echo", args)
		}
	}
}

// echoServer is a one-tool Streamable HTTP MCP server for in-package tests.
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "echo", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(h.Close)
	return h
}
