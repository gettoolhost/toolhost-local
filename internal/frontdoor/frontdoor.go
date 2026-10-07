// Package frontdoor is the driving adapter: the HTTP+MCP surface agents
// connect to. It registers exactly what core.Resolve produced — no lookup,
// no filter at request time; invisible was made uncallable at assembly.
package frontdoor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/core"
)

// Server is the governed /mcp endpoint plus an unauthenticated /healthz.
type Server struct {
	handler http.Handler
	mcp     *mcp.Server
	sink    core.AuditSink

	mu   sync.Mutex // guards live — Reload runs on the watcher goroutine
	live map[string]core.ResolvedTool
}

// Meta wires the gateway's own control-plane tools — registered under the
// reserved toolhost__ namespace and always present regardless of the
// governed surface. They let the agent discover and control tools at
// runtime; approval still gates enablement (enforced in the
// implementation, not here).
type Meta struct {
	Search  func(ctx context.Context, query string) ([]core.ToolInfo, error)
	List    func(ctx context.Context, scope string) ([]core.ToolInfo, error)
	Enable  func(ctx context.Context, names []string) ([]string, error)
	Disable func(ctx context.Context, names []string) ([]string, error)
	// Request files an approval request for unapproved tools — the agent's
	// formal ask; a human still approves. Returns the newly queued names.
	Request func(ctx context.Context, names []string, reason string) ([]string, error)
	// Status reports one qualified name's governance state — used to give
	// precise refusal reasons ("approved but disabled" vs "not approved").
	Status func(name string) (core.ToolInfo, bool)
	// Report returns the live gateway status for toolhost__status.
	Report func() any
	// Audit tails the audit log — the evidence trail of calls, denials, and
	// governance actions — so agents can introspect without file access.
	Audit func(ctx context.Context, limit int, kind string) ([]core.Event, error)
}

// Options controls front-door serving. Stateless is the primary mode
// (SEP-2567 sessionless): no Mcp-Session-Id, each request gets a temporary
// session. Modern clients can receive pushed tools/list_changed through
// subscriptions/listen; older clients served sessionlessly cannot. The
// toolhost__* meta-tools are the pull-based path. Stateful is the compat
// opt-in for older clients that must hold a session for pushed list changes.
// DNS rebinding protection stays on either way (SDK default: non-localhost
// Host headers on localhost addresses get 403).
type Options struct {
	Stateless bool
}

// New builds the front door over a Resolution. Each resolved tool becomes a
// go-sdk registration whose handler closes over its upstream — dispatch is
// wiring, not a lookup.
func New(res *core.Resolution, token string, sink core.AuditSink, meta *Meta, opts *Options) (*Server, error) {
	if token == "" {
		return nil, fmt.Errorf("front door requires a bearer token")
	}
	s := buildServer(res, sink, meta)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.mcp
	}, &mcp.StreamableHTTPOptions{Stateless: opts != nil && opts.Stateless})

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearer(token, sink, mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.handler = mux

	return s, nil
}

// NewBare builds the governed surface with no HTTP wiring — for the stdio
// front door, where the pipe's owner IS the authority (no bearer needed).
func NewBare(res *core.Resolution, sink core.AuditSink, meta *Meta) *Server {
	return buildServer(res, sink, meta)
}

func buildServer(res *core.Resolution, sink core.AuditSink, meta *Meta) *Server {
	if sink == nil {
		sink = discard{}
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "toolhost",
		Version: core.Version,
	}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
		Tools: &mcp.ToolCapabilities{ListChanged: true},
	}})
	s := &Server{mcp: srv, sink: sink, live: map[string]core.ResolvedTool{}}
	for _, rt := range res.Tools {
		s.addTool(rt)
	}
	if meta != nil {
		s.addMetaTools(meta)
	}
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

// ServeStdio speaks MCP over stdin/stdout — the same governed surface as a
// stdio server, for clients that only spawn subprocesses. Blocks until the
// pipe closes or ctx ends. Reloads still land: AddTool/RemoveTools emit
// tools/list_changed down the pipe like any notification.
func (s *Server) ServeStdio(ctx context.Context) error {
	return s.ServeConn(ctx, &mcp.StdioTransport{})
}

// ServeConn serves the governed surface over an arbitrary transport —
// stdio in production, in-process pipes in tests.
func (s *Server) ServeConn(ctx context.Context, t mcp.Transport) error {
	session, err := s.mcp.Connect(ctx, t, nil)
	if err != nil {
		return err
	}
	return session.Wait()
}

// addTool registers one resolved tool; the handler closes over its upstream
// so dispatch is wiring, not a lookup. live is updated under s.mu.
func (s *Server) addTool(rt core.ResolvedTool) {
	s.mcp.AddTool(rt.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		result, err := rt.Upstream.Call(ctx, rt.Origin, req.Params.Arguments)
		ev := core.Event{
			TS:      start,
			Kind:    core.EventToolCall,
			Backend: rt.Upstream.Namespace(),
			Tool:    rt.Qualified,
			MS:      time.Since(start).Milliseconds(),
		}
		if err != nil {
			ev.Err = err.Error()
		}
		s.sink.Record(ev)
		return result, err
	})
	s.live[rt.Qualified] = rt
}

// Reload swaps the served tool set in place: tools removed or re-pointed at
// a reconnected upstream are unregistered, new ones added. The SDK emits
// notifications/tools/list_changed to subscribed modern clients and legacy
// stateful sessions, so they can see the new surface without reconnecting.
func (s *Server) Reload(res *core.Resolution) {
	s.mu.Lock()
	defer s.mu.Unlock()

	want := make(map[string]core.ResolvedTool, len(res.Tools))
	for _, rt := range res.Tools {
		want[rt.Qualified] = rt
	}
	var drop []string
	for name, cur := range s.live {
		if n, ok := want[name]; !ok || n.Upstream != cur.Upstream {
			drop = append(drop, name)
		}
	}
	if len(drop) > 0 {
		s.mcp.RemoveTools(drop...)
		for _, name := range drop {
			delete(s.live, name)
		}
	}
	for name, rt := range want {
		if _, ok := s.live[name]; !ok {
			s.addTool(rt)
		}
	}
}

// addMetaTools registers the toolhost__* control plane. These are never in
// live, so Reload can never drop them — the control plane survives every
// surface swap.
func (s *Server) addMetaTools(m *Meta) {
	srv := s.mcp

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__search",
		Description: "Search toolhost's governed tool catalog by name or description. " +
			"Returns qualified names with approved/enabled state. Approved-but-disabled " +
			"tools can be switched on with toolhost__enable; unapproved tools need a " +
			"human to run `toolhost approve`.",
		InputSchema: objSchema("query", "string", "search text matched against tool names and descriptions"),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		infos, err := m.Search(ctx, a.Query)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(infos), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__list",
		Description: "List governed tools by scope: \"enabled\" (the live surface, default), " +
			"\"approved\" (policy-permitted), or \"all\" (everything discovered).",
		InputSchema: objSchema("scope", "string", `one of "enabled" (default), "approved", "all"`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		infos, err := m.List(ctx, a.Scope)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(infos), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__call",
		Description: "Call a governed tool by qualified name (backend__tool) without it " +
			"being in the listed surface — the escape hatch for clients that don't refresh " +
			"on tools/list_changed. Only enabled tools dispatch; enable first if needed.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "description": "qualified name, backend__tool"},
				"arguments": map[string]any{"type": "object", "description": "arguments for the target tool"},
			},
			"required": []string{"name"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		s.mu.Lock()
		rt, ok := s.live[a.Name]
		s.mu.Unlock()
		if !ok {
			s.sink.Record(core.Event{TS: time.Now(), Kind: core.EventToolForbidden, Tool: a.Name})
			info, found := m.Status(a.Name)
			switch {
			case found && info.Approved:
				return errResult(fmt.Sprintf("%q is approved but disabled — enable it with toolhost__enable", a.Name)), nil
			case found:
				return errResult(fmt.Sprintf("%q is discovered but not approved — a human must run: toolhost approve %s", a.Name, a.Name)), nil
			default:
				return errResult(fmt.Sprintf("unknown tool %q — try toolhost__search", a.Name)), nil
			}
		}
		start := time.Now()
		result, err := rt.Upstream.Call(ctx, rt.Origin, a.Arguments)
		ev := core.Event{
			TS:      start,
			Kind:    core.EventToolCall,
			Backend: rt.Upstream.Namespace(),
			Tool:    rt.Qualified,
			MS:      time.Since(start).Milliseconds(),
		}
		if err != nil {
			ev.Err = err.Error()
		}
		s.sink.Record(ev)
		return result, err
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__enable",
		Description: "Enable approved tools by qualified name — they join the live surface " +
			"immediately (subscribed clients get tools/list_changed). Only approved tools can be " +
			"enabled; approval is a human decision.",
		InputSchema: objSchema("names", "array", "qualified tool names to enable"),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		changed, err := m.Enable(ctx, a.Names)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(map[string]any{"enabled": changed}), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__disable",
		Description: "Disable tools by qualified name — they leave the live surface " +
			"immediately and become uncallable. They stay approved.",
		InputSchema: objSchema("names", "array", "qualified tool names to disable"),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Names []string `json:"names"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		changed, err := m.Disable(ctx, a.Names)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(map[string]any{"disabled": changed}), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__request",
		Description: "Ask for unapproved tools by qualified name — files a request the " +
			"human reviews (toolhost status shows it, toolhost approve answers it). " +
			"Requesting is not approval: the tool stays locked until a human approves.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"names":  map[string]any{"type": "array", "description": "qualified tool names to request"},
				"reason": map[string]any{"type": "string", "description": "why these tools are needed"},
			},
			"required": []string{"names"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Names  []string `json:"names"`
			Reason string   `json:"reason"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		queued, err := m.Request(ctx, a.Names, a.Reason)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(map[string]any{
			"requested": queued,
			"note":      "a human reviews requests — nothing is approved yet",
		}), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__audit",
		Description: "Tail the gateway's audit log — the evidence trail of governed " +
			"calls, refusals, governance actions, and reloads, newest last. " +
			"Read-only; never contains arguments or secrets.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "max events to return (default 50, cap 500)"},
				"kind":  map[string]any{"type": "string", "description": `optional filter: tool_call, auth_failed, backend_error, tool_forbidden, govern, reload`},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Limit int    `json:"limit"`
			Kind  string `json:"kind"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		if m.Audit == nil {
			return errResult("audit log unavailable"), nil
		}
		events, err := m.Audit(ctx, a.Limit, a.Kind)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(events), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "toolhost__status",
		Description: "Report gateway health: serving mode, per-backend state and tool " +
			"counts, approved/enabled/requested totals. Read-only.",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return jsonResult(m.Report()), nil
	})
}

// objSchema is a one-property JSON Schema object — enough for the meta
// tools' single-argument shapes.
func objSchema(name, typ, desc string) map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{name: map[string]any{"type": typ, "description": desc}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errResult(err.Error())
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}

func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// bearer is the whole auth stack: one token, constant-time compared. Denials
// are evidence too — an auth_failed record carries the remote address.
func bearer(token string, sink core.AuditSink, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			sink.Record(core.Event{TS: time.Now(), Kind: core.EventAuthFailed, Remote: r.RemoteAddr})
			w.Header().Set("WWW-Authenticate", `Bearer realm="toolhost"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type discard struct{}

func (discard) Record(core.Event) {}
