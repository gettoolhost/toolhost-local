package upstream

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// An SSE server names the URL its client POSTs to in its first event, and
// that URL can be on any origin. Nothing — credentials or tool arguments —
// may go to an origin other than the one configured.
func TestSSEClientSendsNothingToAnotherOrigin(t *testing.T) {
	const origin = "https://origin.test/sse"
	clients := map[string]func(*testing.T, http.RoundTripper) *http.Client{
		"oauth": func(t *testing.T, rec http.RoundTripper) *http.Client { return oauthSSEClient(t, origin, rec) },
		"static": func(t *testing.T, rec http.RoundTripper) *http.Client {
			return staticSSEClient(t, origin, rec)
		},
	}
	for name, mk := range clients {
		t.Run(name, func(t *testing.T) {
			rec := newRecorder("", "")
			hc := mk(t, rec)
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
				"https://evil.test/messages?session=1", strings.NewReader(`{"jsonrpc":"2.0"}`))
			resp, err := hc.Do(req)
			if err == nil {
				resp.Body.Close()
				t.Errorf("POST to another origin was sent")
			} else if !strings.Contains(err.Error(), "another origin") {
				t.Errorf("error %q doesn't say why", err)
			}
			if h, sent := rec.seen["https://evil.test"]; sent {
				t.Errorf("evil.test received a request, Authorization=%q X-API-Key=%q",
					h.Get("Authorization"), h.Get("X-API-Key"))
			}
			// Same origin still works, with credentials.
			req, _ = http.NewRequestWithContext(context.Background(), http.MethodPost,
				"https://origin.test/messages?session=1", strings.NewReader(`{}`))
			resp, err = hc.Do(req)
			if err != nil {
				t.Fatalf("same-origin POST: %v", err)
			}
			resp.Body.Close()
			if rec.seen["https://origin.test"].Get("X-API-Key") != "fixture-api" {
				t.Errorf("same-origin POST lost its credentials")
			}
		})
	}
}

// staticSSEClient is sseClient for a backend with a static API-key header,
// with its wire swapped for rec.
func staticSSEClient(t *testing.T, origin string, rec http.RoundTripper) *http.Client {
	t.Helper()
	hc, err := sseClient(context.Background(), "a", &config.Backend{
		URL: origin, Headers: map[string]string{"X-API-Key": "fixture-api"},
	}, &Options{})
	if err != nil {
		t.Fatal(err)
	}
	swapWire(t, hc, rec)
	return hc
}
