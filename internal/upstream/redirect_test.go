package upstream

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/oauth"
)

// redirectRecorder is a fake RoundTripper (no sockets): a request to from
// gets a 302 to to; every other URL returns 200. It records the headers
// each scheme://host received.
type redirectRecorder struct {
	from, to string
	seen     map[string]http.Header
}

func newRecorder(from, to string) *redirectRecorder {
	return &redirectRecorder{from: from, to: to, seen: map[string]http.Header{}}
}

func (r *redirectRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.seen[req.URL.Scheme+"://"+req.URL.Host] = req.Header.Clone()
	resp := &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("")), Request: req}
	if req.URL.String() == r.from {
		resp.StatusCode, resp.Status = http.StatusFound, "302 Found"
		resp.Header.Set("Location", r.to)
	}
	return resp, nil
}

// staticClient is baseClient for a backend with a static API-key header and
// a bearer token, with its wire swapped for rec.
func staticClient(t *testing.T, origin string, rec http.RoundTripper) *http.Client {
	t.Helper()
	c := baseClient(&config.Backend{
		URL:     origin,
		Headers: map[string]string{"X-API-Key": "fixture-api"},
		Auth:    &config.Auth{Type: "bearer", Token: "fixture-token"},
	})
	ht, ok := c.Transport.(headerTransport)
	if !ok {
		t.Fatalf("baseClient transport is %T, want headerTransport", c.Transport)
	}
	ht.base = rec
	c.Transport = ht
	return c
}

// oauthSSEClient is sseClient for an oauth backend with a stored, unexpired
// grant (no refresh, no network), with its wire swapped for rec.
func oauthSSEClient(t *testing.T, origin string, rec http.RoundTripper) *http.Client {
	t.Helper()
	store, err := oauth.OpenStore(filepath.Join(t.TempDir(), "toolhost_tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("a", &oauth.StoredToken{AccessToken: "fixture-sse-token", TokenType: "Bearer",
		Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	hc, err := sseClient(context.Background(), "a", &config.Backend{
		URL:     origin,
		Headers: map[string]string{"X-API-Key": "fixture-api"},
		Auth:    &config.Auth{Type: "oauth"},
	}, &Options{Tokens: store})
	if err != nil {
		t.Fatalf("sseClient: %v", err)
	}
	swapWire(t, hc, rec)
	return hc
}

// swapWire replaces the network under an sseClient's transport chain
// (origin guard, then OAuth if any, then static headers) with rec.
func swapWire(t *testing.T, hc *http.Client, rec http.RoundTripper) {
	t.Helper()
	hc.Transport = rewire(t, hc.Transport, rec)
}

func rewire(t *testing.T, rt, rec http.RoundTripper) http.RoundTripper {
	t.Helper()
	switch tr := rt.(type) {
	case originGuard:
		tr.next = rewire(t, tr.next, rec)
		return tr
	case *oauth2.Transport:
		tr.Base = rewire(t, tr.Base, rec)
		return tr
	case headerTransport:
		tr.base = rec
		return tr
	}
	t.Fatalf("unexpected transport %T in the sseClient chain", rt)
	return nil
}

// A same-origin redirect is followed, and the credentials ride along: the
// destination is the server they were configured for.
func TestRedirectSameOriginKeepsCredentials(t *testing.T) {
	for name, build := range map[string]func(*testing.T, string, http.RoundTripper) *http.Client{
		"static": staticClient, "sse_oauth": oauthSSEClient,
	} {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder("https://origin.test/mcp", "https://origin.test/v2/mcp")
			resp, err := build(t, "https://origin.test/mcp", rec).Get("https://origin.test/mcp")
			if err != nil {
				t.Fatalf("same-origin redirect refused: %v", err)
			}
			resp.Body.Close()
			if resp.Request.URL.Path != "/v2/mcp" {
				t.Fatalf("redirect not followed: ended at %s", resp.Request.URL)
			}
			got := rec.seen["https://origin.test"]
			if got.Get("Authorization") == "" || got.Get("X-API-Key") != "fixture-api" {
				t.Fatalf("same-origin destination lost credentials: Authorization=%q X-API-Key=%q",
					got.Get("Authorization"), got.Get("X-API-Key"))
			}
		})
	}
}

// A redirect to a different origin — another host, or the same host over
// plain http — is refused outright: the destination is never contacted, so
// no credential can reach it.
func TestRedirectCrossOriginRefused(t *testing.T) {
	for name, build := range map[string]func(*testing.T, string, http.RoundTripper) *http.Client{
		"static": staticClient, "sse_oauth": oauthSSEClient,
	} {
		for _, to := range []string{"https://other.test/mcp", "http://origin.test/mcp", "https://origin.test:8443/mcp"} {
			t.Run(name+"→"+to, func(t *testing.T) {
				rec := newRecorder("https://origin.test/mcp", to)
				resp, err := build(t, "https://origin.test/mcp", rec).Get("https://origin.test/mcp")
				if err == nil {
					resp.Body.Close()
					t.Fatalf("redirect to %s was followed — credentials reached %v", to, rec.seen)
				}
				if !strings.Contains(err.Error(), "another origin") {
					t.Fatalf("refusal should name the reason, got: %v", err)
				}
				if len(rec.seen) != 1 {
					t.Fatalf("destination was contacted: %v", rec.seen)
				}
			})
		}
	}
}

// Static headers belong to the configured origin. A request the client
// makes elsewhere (the SDK's OAuth discovery hop to an authorization
// server, for one) goes without them.
func TestStaticHeadersScopedToOrigin(t *testing.T) {
	rec := newRecorder("", "")
	resp, err := staticClient(t, "https://origin.test/mcp", rec).Get("https://auth.other.test/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := rec.seen["https://auth.other.test"]
	if got.Get("Authorization") != "" || got.Get("X-API-Key") != "" {
		t.Fatalf("off-origin request carried backend credentials: Authorization=%q X-API-Key=%q",
			got.Get("Authorization"), got.Get("X-API-Key"))
	}
}

// Default ports are one origin with the bare host.
func TestOriginOfDefaultPorts(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"https://h.test/x", "https://h.test:443/y"},
		{"http://H.test/x", "http://h.test:80/"},
	} {
		if originOf(mustURL(t, c.a)) != originOf(mustURL(t, c.b)) {
			t.Errorf("%s and %s should be one origin", c.a, c.b)
		}
	}
	if originOf(mustURL(t, "https://h.test")) == originOf(mustURL(t, "http://h.test")) {
		t.Error("https and http are different origins")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
