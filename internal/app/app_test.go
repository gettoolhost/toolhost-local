package app_test

// End-to-end over a real MCP wire: a fixture upstream behind httptest, the
// governed front door behind httptest, and a real go-sdk client. Proves the
// one invariant this product sells: approved tools are visible AND callable;
// everything else is invisible AND uncallable — plus the evidence record.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolhost/internal/app"
	"toolhost/internal/audit"
	"toolhost/internal/config"
	"toolhost/internal/core"
	"toolhost/internal/frontdoor"
)

type authTransport struct{ token string }

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

func fixtureUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "echoes the message",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}},
	}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]string
		_ = json.Unmarshal(req.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + args["message"]}}}, nil
	})
	srv.AddTool(&mcp.Tool{
		Name:        "secret",
		Description: "should never reach the agent",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "leaked"}}}, nil
	})
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

func TestGovernedCallEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	cfg := &config.File{
		Token: "test-token",
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
		Approved: []string{"up__echo"}, // up__secret deliberately NOT approved
	}

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	sink, err := audit.Open(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()

	ups, errs := app.ConnectBackends(ctx, cfg, nil, sink, nil)
	if len(errs) > 0 {
		t.Fatalf("backend connect failed: %v", errs)
	}
	if len(ups) != 1 {
		t.Fatalf("want 1 upstream, got %d", len(ups))
	}
	defer func() { _ = ups[0].Close() }()

	res, err := core.Resolve(ups, cfg.ApprovedSet(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "up__echo" {
		t.Fatalf("resolve: want exactly up__echo, got %+v", res.Tools)
	}

	fd, err := frontdoor.New(res, cfg.Token, sink)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(fd.Handler())
	defer gw.Close()

	connect := func(token string) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   gw.URL + "/mcp",
			HTTPClient: &http.Client{Transport: authTransport{token: token}},
		}, nil)
	}

	// No token → rejected before any MCP handling.
	resp, err := http.Post(gw.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /mcp: want 401, got %d", resp.StatusCode)
	}

	// Wrong token → same.
	badClient, err := connect("wrong-token")
	if err == nil {
		_ = badClient.Close()
		t.Fatal("connect with wrong token succeeded")
	}

	// Right token → the governed surface.
	session, err := connect("test-token")
	if err != nil {
		t.Fatalf("connect with valid token: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "up__echo" {
		names := []string{}
		for _, tt := range listed.Tools {
			names = append(names, tt.Name)
		}
		t.Fatalf("tools/list: want exactly [up__echo], got %v", names)
	}

	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "up__echo",
		Arguments: json.RawMessage(`{"message":"hi"}`),
	})
	if err != nil {
		t.Fatalf("call up__echo: %v", err)
	}
	text := called.Content[0].(*mcp.TextContent).Text
	if text != "echo:hi" {
		t.Fatalf("call result: want echo:hi, got %q", text)
	}

	// Invisible ⇒ uncallable: the unapproved tool fails on the wire.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "up__secret"}); err == nil {
		t.Fatal("call to unapproved up__secret succeeded")
	}

	// Evidence: one tool_call record carrying the qualified name.
	sink.Close()
	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"kind":"tool_call"`) || !strings.Contains(string(raw), `"tool":"up__echo"`) {
		t.Fatalf("audit log missing the governed call record:\n%s", raw)
	}
	if strings.Contains(string(raw), "up__secret") {
		t.Fatalf("audit log records the unapproved call attempt:\n%s", raw)
	}
}

func TestResolveSkipsUnwireableAndUnapproved(t *testing.T) {
	// A tool whose upstream name can't legally reach the wire is skipped
	// WITH its reason — even when approved. An unapproved tool is neither
	// resolved nor skipped: it is simply never there.
	ups := []core.Upstream{
		&stubUpstream{ns: "a", tools: []*mcp.Tool{
			{Name: "ok"}, {Name: "New Tool"}, {Name: "hidden"},
		}},
	}
	res, err := core.Resolve(ups, map[string]bool{"a__ok": true, "a__New Tool": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "a__ok" {
		t.Fatalf("want exactly a__ok resolved, got %+v", res.Tools)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Tool != "New Tool" {
		t.Fatalf("want New Tool skipped with reason, got %+v", res.Skipped)
	}
}

// The third tier: approved ∩ enabled. A disabled tool is as absent from
// the surface as an unapproved one — invisible AND uncallable.
func TestResolveEnabledSubset(t *testing.T) {
	ups := []core.Upstream{
		&stubUpstream{ns: "a", tools: []*mcp.Tool{{Name: "x"}, {Name: "y"}, {Name: "z"}}},
	}
	approved := map[string]bool{"a__x": true, "a__y": true, "a__z": true}

	res, err := core.Resolve(ups, approved, map[string]bool{"a__x": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "a__x" {
		t.Fatalf("enabled={x}: want exactly a__x, got %+v", res.Tools)
	}

	// An enabled-but-unapproved name contributes nothing — enable is never
	// a backdoor around approval.
	res, err = core.Resolve(ups, map[string]bool{"a__x": true}, map[string]bool{"a__x": true, "a__y": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "a__x" {
		t.Fatalf("enabled={x,y} approved={x}: want exactly a__x, got %+v", res.Tools)
	}
}

type stubUpstream struct {
	ns    string
	tools []*mcp.Tool
}

func (s *stubUpstream) Namespace() string  { return s.ns }
func (s *stubUpstream) Tools() []*mcp.Tool { return s.tools }
func (s *stubUpstream) Close() error       { return nil }
func (s *stubUpstream) Call(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, fmt.Errorf("stub")
}
