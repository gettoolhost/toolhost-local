package frontdoor

// The spec (basic/transports, Streamable HTTP): servers MUST validate the
// Origin header on every incoming connection, and answer 403 when it is
// present and invalid. For a gateway on this machine, the valid origins are
// loopback ones. On a loopback listener, a Host that isn't loopback is a DNS
// rebinding attempt, and it is refused on every route, not only /mcp.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/core"
)

const fixtureToken = "fixture-front-door-token"

func frontDoor(t *testing.T) *httptest.Server {
	t.Helper()
	fd, err := New(&core.Resolution{}, fixtureToken, nil,
		&Meta{Report: func() any { return map[string]string{"ok": "yes"} }}, &Options{Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fd.Handler())
	t.Cleanup(srv.Close)
	return srv
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

func send(t *testing.T, srv *httptest.Server, method, path string, header map[string]string) int {
	t.Helper()
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(initialize)
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fixtureToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestForeignOriginIsForbidden(t *testing.T) {
	srv := frontDoor(t)
	for _, origin := range []string{"http://evil.example", "https://evil.example:8080", "null", "http://127.0.0.1.evil.example"} {
		for _, c := range []struct{ method, path string }{
			{http.MethodPost, "/mcp"}, {http.MethodGet, "/status"}, {http.MethodGet, "/healthz"},
		} {
			if got := send(t, srv, c.method, c.path, map[string]string{"Origin": origin}); got != http.StatusForbidden {
				t.Errorf("%s %s with Origin %q: status %d, want 403", c.method, c.path, origin, got)
			}
		}
	}
}

// The refusal comes before the bearer check: a page on another origin
// learns nothing about the token, and no auth_failed noise is recorded.
func TestForeignOriginIsForbiddenBeforeAuth(t *testing.T) {
	srv := frontDoor(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/status", nil)
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unauthenticated request with a foreign Origin: status %d, want 403", resp.StatusCode)
	}
}

func TestLocalOriginsAndNoOriginAreServed(t *testing.T) {
	srv := frontDoor(t)
	for _, header := range []map[string]string{
		nil,
		{"Origin": "http://localhost:6274"},
		{"Origin": "http://127.0.0.1:5173"},
		{"Origin": "http://[::1]:3000"},
		{"Origin": "https://localhost"},
	} {
		if got := send(t, srv, http.MethodPost, "/mcp", header); got != http.StatusOK {
			t.Errorf("POST /mcp with %v: status %d, want 200", header, got)
		}
		if got := send(t, srv, http.MethodGet, "/status", header); got != http.StatusOK {
			t.Errorf("GET /status with %v: status %d, want 200", header, got)
		}
	}
}

func TestRebindingHostIsForbiddenOnEveryRoute(t *testing.T) {
	srv := frontDoor(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/mcp"}, {http.MethodGet, "/status"}, {http.MethodGet, "/healthz"},
	} {
		if got := send(t, srv, c.method, c.path, map[string]string{"Host": "evil.example:8080"}); got != http.StatusForbidden {
			t.Errorf("%s %s with Host evil.example: status %d, want 403", c.method, c.path, got)
		}
	}
}

// A front door built without a sink still answers a bad token with 401; it
// used to hand the nil sink to the bearer check and panic.
func TestBadTokenWithoutSinkIs401(t *testing.T) {
	srv := frontDoor(t)
	resp, err := http.Get(srv.URL + "/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", resp.StatusCode)
	}
}
