// Package frontdoor is the driving adapter: the HTTP+MCP surface agents
// connect to. It registers exactly what core.Resolve produced — no lookup,
// no filter at request time; invisible was made uncallable at assembly.
package frontdoor

import (
	"context"
	"crypto/subtle"
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

// New builds the front door over a Resolution. Each resolved tool becomes a
// go-sdk registration whose handler closes over its upstream — dispatch is
// wiring, not a lookup.
func New(res *core.Resolution, token string, sink core.AuditSink) (*Server, error) {
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
