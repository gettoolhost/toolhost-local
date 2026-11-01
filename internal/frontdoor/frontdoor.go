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
	"github.com/gettoolhost/toolhost-local/internal/namespace"
)

// Server is the governed /mcp endpoint plus an unauthenticated /healthz.
type Server struct {
	handler http.Handler
	mcp     *mcp.Server
	sink    core.AuditSink
	// onCallErr reports a transport-level dispatch failure back to the
	// gateway — HTTP sessions can stay logically alive after the wire
	// dies, so call errors are a liveness signal too.
	onCallErr func(up core.Upstream, err error)

	mu   sync.Mutex // guards live — Reload runs on the watcher goroutine
	live map[string]core.ResolvedTool
}

// Meta wires the gateway's own control-plane tools — registered under the
// reserved gateway__ namespace and always present regardless of the
// governed surface. They let the agent discover and control tools at
// runtime; approval still gates enablement (enforced in the
// implementation, not here).
type Meta struct {
	// Discover is the agent's catalog view — search and list folded into
	// one revelation verb: query matches name/description, scope narrows
	// ("all" default, "enabled", "approved").
	Discover func(ctx context.Context, query, scope string) ([]core.ToolInfo, error)
	Enable   func(ctx context.Context, names []string) ([]string, error)
	Disable  func(ctx context.Context, names []string) ([]string, error)
	// Request files petitions — unapproved tool names, or whole proposed
	// servers the human may materialize. Returns the newly queued names.
	// A request never widens anything by itself; a human answers it.
	Request func(ctx context.Context, names []string, servers []core.ServerSpec, reason string) ([]string, error)
	// Status reports one qualified name's governance state — used to give
	// precise refusal reasons ("approved but disabled" vs "not approved").
	Status func(name string) (core.ToolInfo, bool)
	// BackendState reports a namespace's liveness for refusal precision:
	// "up", "down" (configured but unreachable — reconnecting), "disabled"
	// (parked by a human), or "" when the namespace isn't configured.
	BackendState func(ns string) string
	// Report returns the live gateway status — served at GET /status for
	// the CLI and harnesses, never exposed as a tool.
	Report func() any
	// CallErr reports a backend Call failure with the session that served
	// it — the gateway uses it to retire dead sessions Wait() can't see.
	CallErr func(up core.Upstream, err error)
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
	if meta != nil && meta.Report != nil {
		// GET /status is the admin read: the CLI's live view and the
		// harness's polling surface. An HTTP endpoint, not a tool — the
		// agent's tools/list stays pure capability.
		mux.Handle("GET /status", bearer(token, sink, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			raw, err := json.Marshal(meta.Report())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(raw)
		})))
	}
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
		Name:    "local-gateway",
		Version: core.Version,
	}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
		Tools: &mcp.ToolCapabilities{ListChanged: true},
	}})
	s := &Server{mcp: srv, sink: sink, live: map[string]core.ResolvedTool{}}
	if meta != nil {
		s.onCallErr = meta.CallErr
	}
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
			if s.onCallErr != nil {
				s.onCallErr(rt.Upstream, err)
			}
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

// addMetaTools registers the gateway__* control plane — five verbs, one
// per tier transition: discover (see), enable (self-serve), disable
// (release), call (invoke), request (petition a human). These are never
// in live, so Reload can never drop them — the control plane survives
// every surface swap.
func (s *Server) addMetaTools(m *Meta) {
	srv := s.mcp

	srv.AddTool(&mcp.Tool{
		Name: "gateway__discover",
		Description: "Discover the governed tool catalog — what exists beyond your live " +
			"surface. Empty query lists everything; a query matches names and descriptions. " +
			"Results carry governance state: enabled (callable now), approved (switch on " +
			"yourself with gateway__enable), discovered (needs a human — file " +
			"gateway__request), denied (a human refused it). Optional scope narrows: " +
			"\"all\" (default), \"enabled\", \"approved\", \"discovered\".",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "match text against tool names and descriptions (empty = everything)"},
				"scope": map[string]any{"type": "string", "description": `optional: "all" (default), "enabled", "approved", "discovered"`},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Query string `json:"query"`
			Scope string `json:"scope"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		infos, err := m.Discover(ctx, a.Query, a.Scope)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(infos), nil
	})

	srv.AddTool(&mcp.Tool{
		Name: "gateway__enable",
		Description: "Enable approved tools by qualified name — they join the live surface " +
			"immediately (subscribed clients get tools/list_changed). Only approved tools can be " +
			"enabled; approval is a human decision. Unapproved names need gateway__request.",
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
		Name: "gateway__disable",
		Description: "Disable tools by qualified name — they leave the live surface " +
			"immediately and become uncallable. They stay approved; re-enable any time.",
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
		Name: "gateway__call",
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
			case found && info.Drifted:
				return errResult(fmt.Sprintf("%q's schema changed since approval — a human must re-approve: toolhost approve %s", a.Name, a.Name)), nil
			case found && info.Denied:
				why := info.DeniedReason
				if why == "" {
					why = "no reason given"
				}
				return errResult(fmt.Sprintf("%q was denied by a human — %s", a.Name, why)), nil
			case found && info.Approved:
				return errResult(fmt.Sprintf("%q is approved but disabled — enable it with gateway__enable", a.Name)), nil
			case found:
				return errResult(fmt.Sprintf("%q is discovered but not approved — ask a human with gateway__request", a.Name)), nil
			default:
				// Not in the catalog — but its backend may exist and be
				// dead or parked, which is a very different answer than
				// "unknown tool".
				if ns, _, err := namespace.Split(a.Name); err == nil {
					switch m.BackendState(ns) {
					case "down":
						return errResult(fmt.Sprintf("backend %q is down — reconnecting; try again shortly", ns)), nil
					case "disabled":
						return errResult(fmt.Sprintf("backend %q is disabled — a human can re-enable it: toolhost mcp enable %s", ns, ns)), nil
					}
				}
				return errResult(fmt.Sprintf("unknown tool %q — try gateway__discover", a.Name)), nil
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
			if s.onCallErr != nil {
				s.onCallErr(rt.Upstream, err)
			}
		}
		s.sink.Record(ev)
		return result, err
	})

	srv.AddTool(&mcp.Tool{
		Name: "gateway__request",
		Description: "Petition a human: unapproved tool names via \"names\", or a whole " +
			"server the gateway should carry via \"servers\" (name + transport + url or " +
			"command/args — topology only, never credentials). Requests queue for human " +
			"review; nothing is granted until they act.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"names":   map[string]any{"type": "array", "description": "qualified tool names to request"},
				"servers": map[string]any{"type": "array", "description": "proposed servers: {name, transport: \"stdio\"|\"http\", url? | command?, args?}"},
				"reason":  map[string]any{"type": "string", "description": "why this is needed — the human reads it"},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var a struct {
			Names   []string          `json:"names"`
			Servers []core.ServerSpec `json:"servers"`
			Reason  string            `json:"reason"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return errResult("bad arguments: " + err.Error()), nil
		}
		if len(a.Names) == 0 && len(a.Servers) == 0 {
			return errResult(`nothing to request — pass "names" (tools) or "servers" (proposals)`), nil
		}
		queued, err := m.Request(ctx, a.Names, a.Servers, a.Reason)
		if err != nil {
			return errResult(err.Error()), nil
		}
		return jsonResult(map[string]any{
			"requested": queued,
			"note":      "a human reviews petitions — nothing is granted yet",
		}), nil
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
