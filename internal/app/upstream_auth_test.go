package app_test

// The upstream transport × auth matrix, proven over real wires: bearer and
// both OAuth modes against an httptest AS+MCP, SSE through the SDK's legacy
// transport, and the token-store round trip that lets serve run unattended.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolhost/internal/config"
	"toolhost/internal/oauth"
	"toolhost/internal/upstream"
)

// mcpFixture is the smallest honest upstream: one echo tool.
func mcpFixture(t *testing.T) *mcp.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "0"}, nil)
	srv.AddTool(&mcp.Tool{
		Name:        "ping",
		Description: "answers pong",
		InputSchema: map[string]any{"type": "object"},
	}, func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil
	})
	return srv
}

func callPing(t *testing.T, b *upstream.Backend) string {
	t.Helper()
	res, err := b.Call(context.Background(), "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call ping: %v", err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

// --- SSE transport ---------------------------------------------------------

func TestSSEUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fixture := mcpFixture(t)
	srv := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return fixture }, nil))
	defer srv.Close()

	b, err := upstream.Connect(ctx, "legacy", &config.Backend{Transport: "sse", URL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("connect sse: %v", err)
	}
	defer b.Close()
	if len(b.Tools()) != 1 || b.Tools()[0].Name != "ping" {
		t.Fatalf("sse discovery: got %+v", b.Tools())
	}
	if got := callPing(t, b); got != "pong" {
		t.Fatalf("sse call: %q", got)
	}
}

// --- bearer / API key ------------------------------------------------------

func TestBearerUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fixture := mcpFixture(t)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil)
	var sawAuth bool
	gated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k3y" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawAuth = true
		mcpHandler.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(gated)
	defer srv.Close()

	b, err := upstream.Connect(ctx, "keyed", &config.Backend{
		Transport: "http", URL: srv.URL,
		Auth: &config.Auth{Type: "bearer", Token: "k3y"},
	}, nil)
	if err != nil {
		t.Fatalf("connect bearer: %v", err)
	}
	defer b.Close()
	if got := callPing(t, b); got != "pong" || !sawAuth {
		t.Fatalf("bearer call: %q (sawAuth=%v)", got, sawAuth)
	}

	// Same upstream without the credential must be refused.
	if _, err := upstream.Connect(ctx, "bare", &config.Backend{Transport: "http", URL: srv.URL}, nil); err == nil {
		t.Fatal("connect without bearer succeeded against a 401 upstream")
	}
}

// --- fake authorization server ---------------------------------------------

// fakeAS is an RFC 9728 + RFC 8414 authorization server + protected resource
// in one httptest mux — enough of the real spec to drive the SDK handlers.
type fakeAS struct {
	t            *testing.T
	srv          *httptest.Server
	token        string // access token /token mints
	tokenCalls   int
	dcrCalls     int
	authorizeHit bool
}

func newFakeAS(t *testing.T, fixture *mcp.Server) *fakeAS {
	f := &fakeAS{t: t, token: "tok-1"}
	mux := http.NewServeMux()

	// Protected resource: unauthenticated → 401 challenge; bearer → MCP.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return fixture }, nil)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.srv.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"resource":              f.srv.URL + "/mcp",
			"authorization_servers": []string{f.srv.URL},
			"scopes_supported":      []string{"tools"},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           f.srv.URL,
			"authorization_endpoint":           f.srv.URL + "/authorize",
			"token_endpoint":                   f.srv.URL + "/token",
			"registration_endpoint":            f.srv.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.authorizeHit = true
		q := r.URL.Query()
		ru, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || q.Get("redirect_uri") == "" {
			http.Error(w, "bad redirect_uri", 400)
			return
		}
		rq := ru.Query()
		rq.Set("code", "authcode-1")
		rq.Set("state", q.Get("state"))
		ru.RawQuery = rq.Encode()
		http.Redirect(w, r, ru.String(), http.StatusFound)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		f.dcrCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  "dyn-client",
			"redirect_uris":              []string{"http://127.0.0.1/callback"},
			"token_endpoint_auth_method": "none",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		var grant = r.Form.Get("grant_type")
		out := map[string]any{"access_token": f.token, "token_type": "Bearer", "expires_in": 3600}
		if grant == "authorization_code" {
			out["refresh_token"] = "rt-1"
		}
		json.NewEncoder(w).Encode(out)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// --- client credentials ----------------------------------------------------

func TestClientCredentialsUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	as := newFakeAS(t, mcpFixture(t))
	b, err := upstream.Connect(ctx, "m2m", &config.Backend{
		Transport: "http", URL: as.srv.URL + "/mcp",
		Auth: &config.Auth{Type: "client_credentials", ClientID: "cid", ClientSecret: "sec"},
	}, nil)
	if err != nil {
		t.Fatalf("connect client_credentials: %v", err)
	}
	defer b.Close()
	if got := callPing(t, b); got != "pong" {
		t.Fatalf("cc call: %q", got)
	}
	if as.tokenCalls == 0 {
		t.Fatal("client_credentials never hit the token endpoint")
	}
}

// --- interactive OAuth (auth code + PKCE) ----------------------------------

func TestOAuthUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	as := newFakeAS(t, mcpFixture(t))

	// Free loopback port for the callback listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redirect := "http://" + ln.Addr().String() + "/callback"
	ln.Close()

	storePath := filepath.Join(t.TempDir(), "toolhost.tokens.json")
	store, err := oauth.OpenStore(storePath)
	if err != nil {
		t.Fatal(err)
	}

	// The "browser": GET the auth URL; the fake AS 302s to the loopback
	// callback, which the fetcher's listener is serving.
	browser := func(u string) error {
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}

	backend := &config.Backend{
		Transport: "http", URL: as.srv.URL + "/mcp",
		Auth: &config.Auth{Type: "oauth", RedirectURL: redirect},
	}
	b, err := upstream.Connect(ctx, "human", backend, &upstream.Options{
		Tokens: store, Interactive: true, OpenURL: browser, Out: io.Discard,
	})
	if err != nil {
		t.Fatalf("connect oauth: %v", err)
	}
	defer b.Close()
	if got := callPing(t, b); got != "pong" {
		t.Fatalf("oauth call: %q", got)
	}
	if !as.authorizeHit || as.dcrCalls == 0 {
		t.Fatalf("oauth flow incomplete: authorizeHit=%v dcrCalls=%d", as.authorizeHit, as.dcrCalls)
	}

	// The grant persisted — and at 0600.
	st := store.Get("human")
	if st == nil || st.AccessToken != "tok-1" || st.RefreshToken != "rt-1" {
		t.Fatalf("stored grant wrong: %+v", st)
	}
	fi, err := os.Stat(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token store mode: want 0600, got %o", fi.Mode().Perm())
	}

	// The serve posture — no interactive fetcher — reuses the stored grant.
	as.authorizeHit = false
	b2, err := upstream.Connect(ctx, "human", backend, &upstream.Options{Tokens: store})
	if err != nil {
		t.Fatalf("reconnect with stored grant: %v", err)
	}
	defer b2.Close()
	if got := callPing(t, b2); got != "pong" {
		t.Fatalf("stored-grant call: %q", got)
	}
	if as.authorizeHit {
		t.Fatal("serve posture ran the browser flow instead of reusing the grant")
	}
}

// A missing grant in serve posture must fail fast with the remedy, not hang.
func TestOAuthMissingGrantFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	as := newFakeAS(t, mcpFixture(t))
	_, err := upstream.Connect(ctx, "human", &config.Backend{
		Transport: "http", URL: as.srv.URL + "/mcp",
		Auth: &config.Auth{Type: "oauth", RedirectURL: "http://127.0.0.1:1/callback"},
	}, &upstream.Options{Tokens: mustStore(t)})
	if err == nil || !strings.Contains(err.Error(), "toolhost auth") {
		t.Fatalf("want 'run toolhost auth' error, got %v", err)
	}
	if as.authorizeHit {
		t.Fatal("non-interactive connect ran the browser flow")
	}
}

// --- SSE + stored OAuth grant ----------------------------------------------

func TestSSEWithStoredGrant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fixture := mcpFixture(t)
	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return fixture }, nil)
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer seed-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		sawAuth = true
		sseHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	store := mustStore(t)
	if err := store.Put("legacy", &oauth.StoredToken{
		AccessToken: "seed-token", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	b, err := upstream.Connect(ctx, "legacy", &config.Backend{
		Transport: "sse", URL: srv.URL,
		Auth: &config.Auth{Type: "oauth"},
	}, &upstream.Options{Tokens: store})
	if err != nil {
		t.Fatalf("connect sse+oauth: %v", err)
	}
	defer b.Close()
	if got := callPing(t, b); got != "pong" || !sawAuth {
		t.Fatalf("sse+oauth call: %q (sawAuth=%v)", got, sawAuth)
	}
}

func mustStore(t *testing.T) *oauth.Store {
	t.Helper()
	s, err := oauth.OpenStore(filepath.Join(t.TempDir(), "toolhost.tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// --- config validation -----------------------------------------------------

func TestAuthConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		backend *config.Backend
		wantErr string
	}{
		{"stdio rejects auth", &config.Backend{Transport: "stdio", Command: "x", Auth: &config.Auth{Type: "bearer", Token: "t"}}, "only valid on http/sse"},
		{"bearer needs token", &config.Backend{Transport: "http", URL: "http://x", Auth: &config.Auth{Type: "bearer"}}, "requires token"},
		{"cc needs both", &config.Backend{Transport: "http", URL: "http://x", Auth: &config.Auth{Type: "client_credentials", ClientID: "i"}}, "client_id and client_secret"},
		{"unknown type", &config.Backend{Transport: "http", URL: "http://x", Auth: &config.Auth{Type: "digest"}}, "bearer, client_credentials, or oauth"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		path := filepath.Join(dir, "toolhost.json")
		f := &config.File{Token: "t", Backends: map[string]*config.Backend{"b": c.backend}}
		if err := f.Save(path); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: want error containing %q, got %v", c.name, c.wantErr, err)
		}
	}
}
