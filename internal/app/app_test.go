package app_test

// End-to-end over a real MCP wire: a fixture upstream behind httptest, the
// governed front door behind httptest, and a real go-sdk client. Proves the
// one invariant this product sells: approved tools are visible AND callable;
// everything else is invisible AND uncallable — plus the evidence record.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/app"
	"github.com/gettoolhost/toolhost-local/internal/audit"
	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
)

type authTransport struct{ token string }

func (t authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if t.token != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token)
	}
	return http.DefaultTransport.RoundTrip(clone)
}

type listenTransport struct {
	token  string
	opened chan struct{}
}

func (t listenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := (authTransport{token: t.token}).RoundTrip(r)
	if err == nil && r.Header.Get("Mcp-Method") == "subscriptions/listen" {
		select {
		case t.opened <- struct{}{}:
		default:
		}
	}
	return resp, err
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
	srv.AddTool(&mcp.Tool{
		Name:        "slow",
		Description: "sleeps 3s — past any test call timeout",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "late"}}}, nil
		}
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
	sink, err := audit.Open(auditPath, 0)
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

	res, err := core.Resolve(ups, cfg.ApprovedSet(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "up__echo" {
		t.Fatalf("resolve: want exactly up__echo, got %+v", res.Tools)
	}

	fd, err := frontdoor.New(res, cfg.Token, sink, nil, nil)
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

func TestStatelessListChangedSubscription(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	up := &stubUpstream{ns: "up", tools: []*mcp.Tool{
		{Name: "one", InputSchema: map[string]any{"type": "object"}},
		{Name: "two", InputSchema: map[string]any{"type": "object"}},
	}}
	resolve := func(approved map[string]bool) *core.Resolution {
		t.Helper()
		res, err := core.Resolve([]core.Upstream{up}, approved, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	fd, err := frontdoor.New(resolve(map[string]bool{"up__one": true}), "test-token", nil, nil,
		&frontdoor.Options{Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(fd.Handler())
	defer gw.Close()

	notifications := make(chan struct{}, 1)
	listening := make(chan struct{}, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case notifications <- struct{}{}:
			default:
			}
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   gw.URL + "/mcp",
		HTTPClient: &http.Client{Transport: listenTransport{token: "test-token", opened: listening}},
	}, &mcp.ClientSessionOptions{ProtocolVersion: "2026-07-28"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("modern client did not open subscriptions/listen")
	}

	fd.Reload(resolve(map[string]bool{"up__one": true, "up__two": true}))
	select {
	case <-notifications:
	case <-ctx.Done():
		t.Fatal("stateless client did not receive tools/list_changed")
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("want 2 tools after reload, got %d", len(listed.Tools))
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
	res, err := core.Resolve(ups, map[string]bool{"a__ok": true, "a__New Tool": true}, nil, nil)
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

	res, err := core.Resolve(ups, approved, map[string]bool{"a__x": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "a__x" {
		t.Fatalf("enabled={x}: want exactly a__x, got %+v", res.Tools)
	}

	// An enabled-but-unapproved name contributes nothing — enable is never
	// a backdoor around approval.
	res, err = core.Resolve(ups, map[string]bool{"a__x": true}, map[string]bool{"a__x": true, "a__y": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "a__x" {
		t.Fatalf("enabled={x,y} approved={x}: want exactly a__x, got %+v", res.Tools)
	}
}

// The gateway's own toolhost__* control plane, over a real Serve: the agent
// searches the catalog, enables an approved-but-disabled tool, calls it
// through the escape hatch, and disables it again — all inside the session.
func TestMetaToolsEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toolhost.json")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	enabled := []string{"up__echo"}
	cfg := &config.File{
		Token:    "test-token",
		Listen:   addr,
		AuditLog: filepath.Join(dir, "audit.jsonl"),
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
		Approved: []string{"up__echo", "up__secret"},
		Enabled:  &enabled,
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, cfgPath, io.Discard, false) }()
	defer func() {
		cancel()
		if err := <-serveDone; err != nil {
			t.Fatalf("serve: %v", err)
		}
	}()

	// Wait for the listener.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/healthz"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: authTransport{token: "test-token"}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range listed.Tools {
		names[tl.Name] = true
	}
	for _, want := range []string{"up__echo", "toolhost__search", "toolhost__list", "toolhost__call",
		"toolhost__enable", "toolhost__disable", "toolhost__request", "toolhost__status"} {
		if !names[want] {
			t.Fatalf("surface missing %q — got %v", want, names)
		}
	}
	if names["up__secret"] {
		t.Fatal("disabled tool leaked into the surface")
	}

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		return res
	}
	text := func(res *mcp.CallToolResult) string {
		return res.Content[0].(*mcp.TextContent).Text
	}

	// Search finds the disabled tool with its governance state.
	res := call("toolhost__search", map[string]any{"query": "secret"})
	if res.IsError || !strings.Contains(text(res), "up__secret") {
		t.Fatalf("search for secret: %v", text(res))
	}

	// The escape hatch refuses a disabled tool — with the remedy.
	res = call("toolhost__call", map[string]any{"name": "up__secret", "arguments": map[string]any{}})
	if !res.IsError || !strings.Contains(text(res), "approved but disabled") {
		t.Fatalf("call disabled tool: want precise refusal, got %v", text(res))
	}

	// Agent enables it at runtime — approval gate holds for unknown names.
	res = call("toolhost__enable", map[string]any{"names": []string{"up__nope"}})
	if !res.IsError || !strings.Contains(text(res), "not approved") {
		t.Fatalf("enable unapproved: want refusal, got %v", text(res))
	}
	res = call("toolhost__enable", map[string]any{"names": []string{"up__secret"}})
	if res.IsError {
		t.Fatalf("enable approved: %v", text(res))
	}

	// Callable via the meta escape hatch without waiting for list_changed.
	res = call("toolhost__call", map[string]any{"name": "up__secret", "arguments": map[string]any{}})
	if res.IsError || text(res) != "leaked" {
		t.Fatalf("call newly-enabled tool: got %v", text(res))
	}

	// The enablement persisted to the config — same file the CLI edits.
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsEnabled("up__secret") {
		t.Fatal("agent enable did not persist to toolhost.json")
	}

	// And back off again — runtime governance in both directions.
	res = call("toolhost__disable", map[string]any{"names": []string{"up__secret"}})
	if res.IsError {
		t.Fatalf("disable: %v", text(res))
	}
	res = call("toolhost__call", map[string]any{"name": "up__secret", "arguments": map[string]any{}})
	if !res.IsError {
		t.Fatal("disabled tool still callable via toolhost__call")
	}
}

// Stateless mode (SEP-2567) is the default: the front door serves without
// session IDs — each request is independent. Init, list, and call still
// work end to end, including the boundary case that broke before: a
// new-protocol client against a legacy-protocol upstream (transport values
// must not leak across the gateway). No mode field set — this exercises
// the default, not an explicit opt-in.
func TestStatelessEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toolhost.json")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := &config.File{
		Token:    "test-token",
		Listen:   addr,
		AuditLog: filepath.Join(dir, "audit.jsonl"),
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
		Approved: []string{"up__echo"},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, cfgPath, io.Discard, false) }()
	defer func() {
		cancel()
		if err := <-serveDone; err != nil {
			t.Fatalf("serve: %v", err)
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/healthz"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Sessionless on the wire: no Mcp-Session-Id is issued.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"t","version":"0"},"capabilities":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize: want 200, got %d", resp.StatusCode)
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.Fatalf("stateless server issued a session id: %q", sid)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: authTransport{token: "test-token"}},
	}, nil)
	if err != nil {
		t.Fatalf("stateless connect: %v", err)
	}
	defer session.Close()

	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "up__echo",
		Arguments: map[string]any{"message": "hi"},
	})
	if err != nil {
		t.Fatalf("stateless call: %v", err)
	}
	if text := called.Content[0].(*mcp.TextContent).Text; text != "echo:hi" {
		t.Fatalf("want echo:hi, got %q", text)
	}
}

// Passthrough (federation): an upstream whose names arrive already
// qualified keeps them — no re-namespacing; the inner gateway's
// toolhost__* control plane is dropped; unqualified names are skipped.
func TestResolvePassthrough(t *testing.T) {
	ups := []core.Upstream{
		&stubPassthroughUpstream{stubUpstream{ns: "edge", tools: []*mcp.Tool{
			{Name: "cbm__search_graph"}, // already qualified — passes through
			{Name: "toolhost__search"},  // inner control plane — dropped
			{Name: "bare_name"},         // not qualified — skipped
		}}},
	}
	res, err := core.Resolve(ups, map[string]bool{"cbm__search_graph": true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Qualified != "cbm__search_graph" {
		t.Fatalf("want exactly cbm__search_graph, got %+v", res.Tools)
	}
	if res.Tools[0].Origin != "cbm__search_graph" {
		t.Fatalf("origin must be the qualified name for dispatch, got %q", res.Tools[0].Origin)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("want toolhost__search + bare_name skipped, got %+v", res.Skipped)
	}
}

type stubPassthroughUpstream struct{ stubUpstream }

func (s *stubPassthroughUpstream) Passthrough() bool { return true }

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

// call_timeout: the fixture's slow tool sleeps 3s — returning inside 2s
// proves a bound fired. Three layers: the shared default, a per-backend
// override, and the caller's own deadline (which must still win).
func TestCallTimeout(t *testing.T) {
	up := fixtureUpstream(t)

	connect := func(t *testing.T, backendTimeout string, defaultTimeout time.Duration) *upstream.Backend {
		t.Helper()
		cfg := &config.Backend{Transport: "http", URL: up.URL, CallTimeout: backendTimeout}
		b, err := upstream.Connect(context.Background(), "up", cfg,
			&upstream.Options{DefaultCallTimeout: defaultTimeout})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	call := func(ctx context.Context, b *upstream.Backend) time.Duration {
		t.Helper()
		start := time.Now()
		if _, err := b.Call(ctx, "slow", nil); err == nil {
			t.Fatal("call to 30s-sleeping tool succeeded")
		}
		return time.Since(start)
	}

	t.Run("default bound", func(t *testing.T) {
		b := connect(t, "", 150*time.Millisecond)
		if d := call(context.Background(), b); d > 2*time.Second {
			t.Fatalf("default call_timeout not enforced — call ran %v", d)
		}
	})

	t.Run("per-backend override", func(t *testing.T) {
		b := connect(t, "150ms", 30*time.Second)
		if d := call(context.Background(), b); d > 2*time.Second {
			t.Fatalf("backend call_timeout ignored — call ran %v", d)
		}
	})

	t.Run("caller deadline wins", func(t *testing.T) {
		b := connect(t, "", 30*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		if d := call(ctx, b); d > 2*time.Second {
			t.Fatalf("caller's tighter deadline lost to call_timeout — call ran %v", d)
		}
	})
}

// The agent's formal ask: toolhost__request queues unapproved names in the
// config without widening the surface; a human approve answers (and
// consumes) the request. Over the real wire.
func TestRequestEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toolhost.json")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	cfg := &config.File{
		Token:    "test-token",
		Listen:   addr,
		AuditLog: filepath.Join(dir, "audit.jsonl"),
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
		Approved: []string{"up__echo"}, // up__secret discovered but NOT approved
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	serveDone := make(chan error, 1)
	go func() { serveDone <- app.Serve(ctx, cfgPath, io.Discard, false) }()
	defer func() {
		cancel()
		if err := <-serveDone; err != nil {
			t.Fatalf("serve: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := http.Get("http://" + addr + "/healthz"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("serve never came up")
		}
		time.Sleep(50 * time.Millisecond)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + addr + "/mcp",
		HTTPClient: &http.Client{Transport: authTransport{token: "test-token"}},
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("call %s: %v", name, err)
		}
		return res
	}
	text := func(res *mcp.CallToolResult) string {
		return res.Content[0].(*mcp.TextContent).Text
	}

	// Requesting an already-approved tool is refused — the queue is for
	// asks, not approvals-that-exist.
	res := call("toolhost__request", map[string]any{"names": []string{"up__echo"}})
	if !res.IsError || !strings.Contains(text(res), "already approved") {
		t.Fatalf("request approved tool: want refusal, got %v", text(res))
	}

	// The real ask lands in the config — and the tool stays uncallable.
	res = call("toolhost__request", map[string]any{
		"names":  []string{"up__secret"},
		"reason": "need it for the task",
	})
	if res.IsError {
		t.Fatalf("request: %v", text(res))
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.RequestedSet()["up__secret"] {
		t.Fatalf("request did not persist to config: %+v", reloaded.Requested)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "up__secret"}); err == nil {
		t.Fatal("requested tool is callable — a request must not widen the surface")
	}

	// Same ask again → deduped, not doubled.
	res = call("toolhost__request", map[string]any{"names": []string{"up__secret"}})
	if res.IsError || strings.Contains(text(res), `"up__secret",`) {
		t.Fatalf("dedup: second request should add nothing, got %v", text(res))
	}
	reloaded, _ = config.Load(cfgPath)
	if len(reloaded.Requested) != 1 {
		t.Fatalf("dedup: want 1 pending request, got %+v", reloaded.Requested)
	}

	// toolhost__status reports the pending ask to the human.
	res = call("toolhost__status", map[string]any{})
	if res.IsError {
		t.Fatalf("status: %v", text(res))
	}
	var report struct {
		Mode      string                  `json:"mode"`
		Approved  int                     `json:"approved"`
		Requested []struct{ Name string } `json:"requested"`
		Backends  map[string]struct {
			Up    bool `json:"up"`
			Tools int  `json:"tools"`
		} `json:"backends"`
	}
	if err := json.Unmarshal([]byte(text(res)), &report); err != nil {
		t.Fatalf("status payload: %v\n%s", err, text(res))
	}
	if report.Approved != 1 || !report.Backends["up"].Up || report.Backends["up"].Tools != 3 {
		t.Fatalf("status report wrong: %s", text(res))
	}
	if len(report.Requested) != 1 || report.Requested[0].Name != "up__secret" {
		t.Fatalf("status missing the pending request: %s", text(res))
	}

	// The human answers: approve consumes the request and the tool lands
	// on the surface.
	if err := app.EditApprovals(ctx, cfgPath, []string{"up__secret"}, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	reloaded, _ = config.Load(cfgPath)
	if len(reloaded.Requested) != 0 {
		t.Fatalf("approve left a stale request: %+v", reloaded.Requested)
	}
	// The watcher picks up the approval — poll the call itself until the
	// tool lands on the surface (search reflects config, not the swap).
	deadline = time.Now().Add(10 * time.Second)
	for {
		called, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "up__secret"})
		if err == nil {
			if text(called) != "leaked" {
				t.Fatalf("want leaked, got %q", text(called))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("approved tool never became callable: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Approving one tool must pin exactly that tool — never its siblings on
// the same backend (regression: schemaPins once collected every tool a
// candidate backend exposed, so approving fs__read approved fs__write too).
func TestApprovePinsOnlyRequestedTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toolhost.json")
	cfg := &config.File{
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}

	if err := app.EditApprovals(ctx, cfgPath, []string{"up__echo"}, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsApproved("up__echo") || reloaded.ApprovedSchemas["up__echo"] == "" {
		t.Fatalf("up__echo should be approved+pinned: %+v / %+v", reloaded.Approved, reloaded.ApprovedSchemas)
	}
	for _, sib := range []string{"up__secret", "up__slow"} {
		if reloaded.IsApproved(sib) || reloaded.ApprovedSchemas[sib] != "" {
			t.Fatalf("%s must not be approved/pinned by a sibling approval", sib)
		}
	}
}

// serve --stdio: the same governed surface over a plain pipe. The test
// drives it through IOTransport — identical newline-delimited JSON, no
// subprocess needed.
func TestStdioServeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	up := fixtureUpstream(t)
	cfg := &config.File{
		Backends: map[string]*config.Backend{
			"up": {Transport: "http", URL: up.URL},
		},
		Approved: []string{"up__echo"},
	}

	ups, errs := app.ConnectBackends(ctx, cfg, nil, nil, nil)
	if len(errs) > 0 || len(ups) != 1 {
		t.Fatalf("connect: %v", errs)
	}
	defer func() { _ = ups[0].Close() }()

	res, err := core.Resolve(ups, cfg.ApprovedSet(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := frontdoor.NewBare(res, nil, nil)

	// Two pipes: server reads clientWrites→serverReads, writes back the
	// other way. IOTransport is the same framing StdioTransport uses.
	serverR, clientW := io.Pipe()
	clientR, serverW := io.Pipe()
	go func() {
		_ = srv.ServeConn(ctx, &mcp.IOTransport{Reader: serverR, Writer: serverW})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil)
	if err != nil {
		t.Fatalf("stdio connect: %v", err)
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "up__echo" {
		t.Fatalf("stdio surface: want [up__echo], got %+v", listed.Tools)
	}
	called, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "up__echo",
		Arguments: json.RawMessage(`{"message":"pipe"}`),
	})
	if err != nil {
		t.Fatalf("stdio call: %v", err)
	}
	if text := called.Content[0].(*mcp.TextContent).Text; text != "echo:pipe" {
		t.Fatalf("want echo:pipe, got %q", text)
	}
}
