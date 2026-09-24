// Package upstream is the driven adapter for backend MCP servers: it turns
// a config.Backend into a live core.Upstream over the go-sdk client. stdio
// spawns a subprocess (CommandTransport); http dials a streamable-HTTP
// endpoint (StreamableClientTransport).
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolhost/internal/config"
	"toolhost/internal/core"
)

// maxDiscoveryItems bounds a single backend's tool list so a malfunctioning
// or malicious server can't stream synthetic pages indefinitely.
const maxDiscoveryItems = 10000

// Backend is a connected upstream MCP server.
type Backend struct {
	namespace string
	session   *mcp.ClientSession
	tools     []*mcp.Tool
}

// Connect dials (or spawns) the backend and discovers its tool inventory.
func Connect(ctx context.Context, name string, cfg *config.Backend) (*Backend, error) {
	transport, err := transportFor(cfg)
	if err != nil {
		return nil, fmt.Errorf("backend %q: %w", name, err)
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "toolhost-upstream",
		Version: core.Version,
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("backend %q: connect: %w", name, err)
	}

	tools, err := listAllTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("backend %q: list tools: %w", name, err)
	}

	return &Backend{namespace: name, session: session, tools: tools}, nil
}

func (b *Backend) Namespace() string  { return b.namespace }
func (b *Backend) Tools() []*mcp.Tool { return b.tools }

func (b *Backend) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return b.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
}

func (b *Backend) Close() error {
	if b == nil || b.session == nil {
		return nil
	}
	return b.session.Close()
}

var _ core.Upstream = (*Backend)(nil)

func transportFor(cfg *config.Backend) (mcp.Transport, error) {
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio transport requires command")
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		// Inherit the caller's environment so upstream servers see the
		// user's own API keys etc.; configured env wins on collision.
		if len(cfg.Env) > 0 {
			env := os.Environ()
			for k, v := range cfg.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		return &mcp.CommandTransport{Command: cmd}, nil

	case "http", "streamable_http":
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("http transport requires an http(s) url, got %q", cfg.URL)
		}
		return &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: cfg.Headers}},
		}, nil

	default:
		return nil, fmt.Errorf("transport must be %q or %q, got %q", "stdio", "http", cfg.Transport)
	}
}

// headerTransport injects per-backend static headers (e.g. an upstream API
// key) on every request the MCP client makes.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if len(t.headers) == 0 {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		if !strings.EqualFold(k, "Host") {
			clone.Header.Set(k, v)
		}
	}
	return base.RoundTrip(clone)
}

// listAllTools pages through the upstream tool list.
func listAllTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	cursor := ""
	for {
		result, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		tools = append(tools, result.Tools...)
		if len(tools) > maxDiscoveryItems {
			return nil, fmt.Errorf("upstream tool list exceeds %d items — refusing", maxDiscoveryItems)
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
}
