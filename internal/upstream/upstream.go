// Package upstream is the driven adapter for backend MCP servers: it turns
// a config.Backend into a live core.Upstream over the go-sdk client.
//
// Transports: stdio spawns a subprocess (CommandTransport); http and sse
// dial remote endpoints (StreamableClientTransport / SSEClientTransport).
// Upstream auth — none, bearer, client_credentials, interactive OAuth — is
// assembled here from the backend's auth block; on streamable the SDK
// drives the OAuthHandler itself, on SSE we drive it once at connect.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"toolhost/internal/config"
	"toolhost/internal/core"
	"toolhost/internal/oauth"
)

// maxDiscoveryItems bounds a single backend's tool list so a malfunctioning
// or malicious server can't stream synthetic pages indefinitely.
const maxDiscoveryItems = 10000

// Options carries the cross-cutting things a connect needs that config
// doesn't know: where tokens persist and whether a flow may open a browser.
type Options struct {
	// Tokens is the persistent upstream-token store. Nil means no stored
	// grants — an oauth backend then can only authorize interactively.
	Tokens *oauth.Store
	// Interactive permits the oauth fetcher to open a browser and wait on a
	// loopback callback (`toolhost auth`); serve/discover pass false so a
	// missing grant fails fast instead of surprising a terminal.
	Interactive bool
	// OpenURL launches the authorization URL — nil uses the platform opener.
	// Tests inject a redirect-following HTTP GET.
	OpenURL func(string) error
	// Out is where the interactive fetcher prints the authorization URL.
	Out io.Writer
	// DefaultCallTimeout bounds upstream calls for backends without their
	// own call_timeout. Zero means rely on the caller's deadline alone.
	DefaultCallTimeout time.Duration
}

// Backend is a connected upstream MCP server.
type Backend struct {
	namespace   string
	session     *mcp.ClientSession
	tools       []*mcp.Tool
	passthrough bool
	callTimeout time.Duration
	opts        *Options
}

// Passthrough reports whether this upstream is a federated gateway — its
// tool names are already qualified. Part of core.Passthrough.
func (b *Backend) Passthrough() bool { return b.passthrough }

// Connect dials (or spawns) the backend and discovers its tool inventory.
func Connect(ctx context.Context, name string, cfg *config.Backend, opts *Options) (*Backend, error) {
	if opts == nil {
		opts = &Options{}
	}
	transport, err := transportFor(ctx, name, cfg, opts)
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

	// Per-backend override is parsed once; the shared default is read from
	// opts per call so a hot-reloaded call_timeout reaches even unchanged
	// sessions.
	var timeout time.Duration
	if cfg.CallTimeout != "" {
		timeout, _ = time.ParseDuration(cfg.CallTimeout)
	}
	return &Backend{namespace: name, session: session, tools: tools,
		passthrough: cfg.Passthrough, callTimeout: timeout, opts: opts}, nil
}

func (b *Backend) Namespace() string  { return b.namespace }
func (b *Backend) Tools() []*mcp.Tool { return b.tools }

func (b *Backend) Call(ctx context.Context, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	dctx, cancel := detached(ctx)
	defer cancel()
	// The per-backend override wins; the shared default is read live so a
	// hot-reloaded call_timeout reaches sessions that were kept.
	timeout := b.callTimeout
	if timeout <= 0 && b.opts != nil {
		timeout = b.opts.DefaultCallTimeout
	}
	if timeout > 0 {
		var tcancel context.CancelFunc
		dctx, tcancel = context.WithTimeout(dctx, timeout)
		defer tcancel()
	}
	return b.session.CallTool(dctx, &mcp.CallToolParams{
		Name:      tool,
		Arguments: args,
	})
}

// detached returns a context carrying ctx's deadline and cancellation but
// none of its values. Request-scoped values are transport-local: the SDK
// stores the downstream client's negotiated protocol version in the request
// ctx, and letting it cross into an upstream call would stamp that version
// onto requests to a server that may have negotiated something older —
// rejected as Bad Request. Lifetime crosses the gateway boundary; values
// never do.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithCancel(context.Background())
	if dl, ok := ctx.Deadline(); ok {
		dctx, cancel = context.WithDeadline(context.Background(), dl)
	}
	stop := context.AfterFunc(ctx, cancel)
	return dctx, func() { stop(); cancel() }
}

func (b *Backend) Close() error {
	if b == nil || b.session == nil {
		return nil
	}
	return b.session.Close()
}

var _ core.Upstream = (*Backend)(nil)

func transportFor(ctx context.Context, name string, cfg *config.Backend, opts *Options) (mcp.Transport, error) {
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
		if err := validateHTTPURL(cfg.URL); err != nil {
			return nil, err
		}
		handler, err := oauth.NewHandler(ctx, name, cfg.Auth, opts.Tokens, opts.Interactive, opts.OpenURL, opts.Out)
		if err != nil {
			return nil, err
		}
		return &mcp.StreamableClientTransport{
			Endpoint:     cfg.URL,
			HTTPClient:   baseClient(cfg),
			OAuthHandler: handler,
		}, nil

	case "sse":
		if err := validateHTTPURL(cfg.URL); err != nil {
			return nil, err
		}
		// SSEClientTransport has no OAuthHandler seam — drive the grant
		// ourselves, once, at connect.
		hc, err := sseClient(ctx, name, cfg, opts)
		if err != nil {
			return nil, err
		}
		return &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil

	default:
		return nil, fmt.Errorf("transport must be %q, %q or %q, got %q", "stdio", "http", "sse", cfg.Transport)
	}
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("requires an http(s) url, got %q", raw)
	}
	return nil
}

// baseClient returns the HTTP client every remote transport shares: static
// headers plus the auth.bearer sugar (auth wins over a literal Authorization
// in headers — configured credentials beat loose strings).
func baseClient(cfg *config.Backend) *http.Client {
	headers := make(map[string]string, len(cfg.Headers)+1)
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	if cfg.Auth != nil && cfg.Auth.Type == "bearer" && cfg.Auth.Token != "" {
		headers["Authorization"] = "Bearer " + cfg.Auth.Token
	}
	return &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: headers}}
}

// sseClient builds the HTTP client for an SSE backend, resolving auth the
// SDK can't: client_credentials drives its own grant against the probed 401,
// oauth reuses the stored grant (or points at `toolhost auth`).
func sseClient(ctx context.Context, name string, cfg *config.Backend, opts *Options) (*http.Client, error) {
	base := headerTransport{base: http.DefaultTransport, headers: cfg.Headers}
	plain := &http.Client{Transport: base}

	if cfg.Auth == nil {
		return plain, nil
	}
	switch cfg.Auth.Type {
	case "", "none", "bearer":
		return baseClient(cfg), nil

	case "client_credentials":
		h, err := oauth.NewHandler(ctx, name, cfg.Auth, opts.Tokens, false, opts.OpenURL, opts.Out)
		if err != nil {
			return nil, err
		}
		if err := oauth.AuthorizeOnce(ctx, cfg.URL, h, plain); err != nil {
			return nil, err
		}
		ts, err := h.TokenSource(ctx)
		if err != nil {
			return nil, err
		}
		return &http.Client{Transport: &oauth2.Transport{Source: ts, Base: base}}, nil

	case "oauth":
		if opts.Tokens == nil {
			return nil, fmt.Errorf("backend %q needs an OAuth grant — run: toolhost auth %s", name, name)
		}
		ts := opts.Tokens.Source(ctx, name)
		if ts == nil {
			return nil, fmt.Errorf("backend %q needs an OAuth grant — run: toolhost auth %s", name, name)
		}
		return &http.Client{Transport: &oauth2.Transport{Source: ts, Base: base}}, nil
	}
	return plain, nil
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
