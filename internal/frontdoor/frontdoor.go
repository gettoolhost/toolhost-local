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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolhost/internal/core"
)

// Server is the governed /mcp endpoint plus an unauthenticated /healthz.
type Server struct {
	handler http.Handler
	mcp     *mcp.Server
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

	for _, rt := range res.Tools {
		rt := rt
		srv.AddTool(rt.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
			sink.Record(ev)
			return result, err
		})
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

	return &Server{handler: mux, mcp: srv}, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

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
