package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// LoopbackFetcher implements auth.AuthorizationCodeFetcher the way CLI OAuth
// always works: bind the loopback address the AS will redirect to, hand the
// authorization URL to the user's browser (and print it — the open is
// best-effort), wait for the callback carrying code+state(+iss per RFC 9207).
func LoopbackFetcher(redirectURL string, open func(string) error, out io.Writer) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		u, err := url.Parse(redirectURL)
		if err != nil {
			return nil, fmt.Errorf("auth.redirect_url %q: %w", redirectURL, err)
		}
		host := u.Host
		if u.Port() == "" {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
		ln, err := net.Listen("tcp", host)
		if err != nil {
			return nil, fmt.Errorf("oauth callback listener on %s: %w — set auth.redirect_url to a free loopback port", host, err)
		}
		defer ln.Close()

		resCh := make(chan *auth.AuthorizationResult, 1)
		errCh := make(chan error, 1)
		mux := http.NewServeMux()
		mux.HandleFunc(u.Path, func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				errCh <- fmt.Errorf("authorization failed: %s %s", e, q.Get("error_description"))
				fmt.Fprint(w, "authorization failed — you can close this tab")
				return
			}
			resCh <- &auth.AuthorizationResult{
				Code:  q.Get("code"),
				State: q.Get("state"),
				Iss:   q.Get("iss"),
			}
			fmt.Fprint(w, "toolhost: authorized — you can close this tab")
		})
		srv := &http.Server{Handler: mux}
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()

		fmt.Fprintf(out, "open this URL to authorize:\n\n  %s\n\n", args.URL)
		if open != nil {
			_ = open(args.URL)
		}

		select {
		case res := <-resCh:
			return res, nil
		case err := <-errCh:
			return nil, err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Minute):
			return nil, errors.New("authorization timed out waiting for the browser callback")
		}
	}
}

// defaultOpen picks the platform's URL opener. Best-effort — the URL is
// always printed, so a missing opener only costs a copy-paste.
func defaultOpen(rawURL string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL)
	default:
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}
