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

	"toolhost/internal/core"
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
	// Status reports one qualified name's governance state — used to give
	// precise refusal reasons ("approved but disabled" vs "not approved").
	Status func(name string) (core.ToolInfo, bool)
}

// New builds the front door over a Resolution. Each resolved tool becomes a
// go-sdk registration whose handler closes over its upstream — dispatch is
// wiring, not a lookup.
func New(res *core.Resolution, token string, sink core.AuditSink, meta *Meta) (*Server, error) {
	if token == "" {
		return nil, fmt.Errorf("front door requires a bearer token")
	}
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

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", bearer(token, sink, mcpHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	s.handler = mux

	return s, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

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
// notifications/tools/list_changed, so connected agents see the new surface
// without reconnecting.
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
			"immediately (clients get tools/list_changed). Only approved tools can be " +
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
