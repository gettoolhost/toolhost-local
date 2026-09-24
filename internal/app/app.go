// Package app wires the ports to the adapters: the use cases behind each
// CLI command. Nothing here knows HTTP flags or argv — cmd/toolhost owns
// that edge.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"time"

	"toolhost/internal/audit"
	"toolhost/internal/config"
	"toolhost/internal/core"
	"toolhost/internal/frontdoor"
	"toolhost/internal/oauth"
	"toolhost/internal/upstream"
)

// connectTimeout bounds each backend's connect+discover handshake.
const connectTimeout = 30 * time.Second

// Init writes a fresh config with a generated bearer token. It refuses to
// overwrite — the token is a credential, and clobbering it silently is how
// "my gateway stopped working" bugs are born.
func Init(path string, w io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (refusing to overwrite — delete it first if you mean it)", path)
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	f := &config.File{
		Listen:   config.DefaultListen,
		Token:    token,
		AuditLog: config.DefaultAuditLog,
		Backends: map[string]*config.Backend{},
		Approved: []string{},
	}
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s\n\nnext: add a backend to \"backends\", then:\n  toolhost discover    # see every tool it exposes\n  toolhost approve fs__read_file\n  toolhost serve       # %s/mcp, Authorization: Bearer <token>\n", path, f.Listen)
	return nil
}

func newToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "th_" + hex.EncodeToString(buf), nil
}

// Discover connects every backend and prints its tool inventory, marking
// which qualified names are approved. Read-only: it writes nothing, audits
// nothing.
func Discover(ctx context.Context, path string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	ups, errs := ConnectBackends(ctx, f, tokens, audit.Discard{}, slog.Default())
	defer closeAll(ups)

	names := make([]string, 0, len(ups))
	for _, up := range ups {
		names = append(names, up.Namespace())
	}
	for _, name := range sortedKeys(errs) {
		fmt.Fprintf(w, "%-14s unreachable: %s\n", name, errs[name])
	}
	if len(names) == 0 && len(errs) == 0 {
		fmt.Fprintln(w, "no backends configured — edit backends in "+path)
		return nil
	}

	for _, up := range ups {
		for _, t := range up.Tools() {
			qualified := up.Namespace() + "__" + t.Name
			status := " "
			if f.IsEnabled(qualified) {
				status = "✓"
			} else if f.IsApproved(qualified) {
				status = "○" // approved but disabled — policy kept, surface off
			}
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Fprintf(w, "%-2s %-40s %s\n", status, qualified, desc)
		}
	}
	fmt.Fprintln(w, "\n✓ = enabled · ○ = approved but disabled · blank = discovered only")
	fmt.Fprintln(w, "toolhost approve/enable/disable <qualified-name>")
	return nil
}

// EditApprovals adds (approve=true) or removes qualified names from config.
func EditApprovals(path string, names []string, approve bool, w io.Writer) error {
	if len(names) == 0 {
		return errors.New("name at least one qualified tool, e.g. toolhost approve fs__read_file")
	}
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	var changed []string
	if approve {
		if changed, err = f.Approve(names...); err != nil {
			return err
		}
	} else {
		changed = f.Revoke(names...)
	}
	if len(changed) == 0 {
		fmt.Fprintln(w, "no change")
		return nil
	}
	if err := f.Save(path); err != nil {
		return err
	}
	verb := "approved"
	if !approve {
		verb = "revoked"
	}
	for _, n := range changed {
		fmt.Fprintf(w, "%s %s\n", verb, n)
	}
	fmt.Fprintln(w, "takes effect on next serve")
	return nil
}

// EditEnabled adjusts the live surface. enable adds names to the allowlist
// (no-op until a list exists — without one, all approved tools are already
// enabled); disable hides approved tools, materializing the allowlist as
// approved ∖ names on first use. only=true replaces the list; names=nil
// with enable resets to "all approved" (the --all form).
func EditEnabled(path string, names []string, enable, only bool, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	var changed []string
	var verb string
	switch {
	case enable && len(names) == 0 && !only:
		if f.Enabled == nil {
			fmt.Fprintln(w, "all approved tools already enabled")
			return nil
		}
		f.EnableAll()
		verb = "enabled every approved tool (allowlist cleared)"
	case enable && only:
		if len(names) == 0 {
			return errors.New("enable --only needs at least one qualified tool")
		}
		changed, err = f.EnableOnly(names...)
		verb = "enabled (only these)"
	case enable:
		if f.Enabled == nil {
			fmt.Fprintln(w, "already enabled — no allowlist is set, so every approved tool is on")
			return nil
		}
		changed, err = f.Enable(names...)
		verb = "enabled"
	default:
		if len(names) == 0 {
			return errors.New("name at least one qualified tool, e.g. toolhost disable neon__list_operations")
		}
		changed, err = f.Disable(names...)
		verb = "disabled"
	}
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(names) > 0 {
		fmt.Fprintln(w, "no change")
		return nil
	}
	if err := f.Save(path); err != nil {
		return err
	}
	for _, n := range changed {
		fmt.Fprintf(w, "%s %s\n", verb, n)
	}
	if len(changed) == 0 {
		fmt.Fprintln(w, verb)
	}
	fmt.Fprintln(w, "takes effect on next serve")
	return nil
}

// Auth runs the upstream grant for one backend — the OAuth browser dance or
// the client_credentials exchange — driven explicitly so it works the same
// for streamable and SSE upstreams. The minted token persists to the token
// store; serve reuses (and refreshes) it without ever prompting.
func Auth(ctx context.Context, path, name string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[name]
	if !ok {
		return fmt.Errorf("no backend %q in %s", name, path)
	}
	if b.Transport == "stdio" {
		return fmt.Errorf("backend %q is stdio — credentials belong in its env block", name)
	}
	if b.Auth == nil || (b.Auth.Type != "oauth" && b.Auth.Type != "client_credentials") {
		return fmt.Errorf("backend %q has no oauth/client_credentials auth block — bearer tokens go in config directly", name)
	}
	store, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	h, err := oauth.NewHandler(ctx, name, b.Auth, store, true, nil, w)
	if err != nil {
		return err
	}
	if err := oauth.AuthorizeOnce(ctx, b.URL, h, http.DefaultClient); err != nil {
		return err
	}
	fmt.Fprintf(w, "authorized %s — token stored in %s\n", name, store.Path())
	return nil
}

// Logout drops a backend's stored upstream grant — the counterpart of
// `toolhost auth`. Serve then fails closed on that backend until the next
// `toolhost auth <backend>`.
func Logout(path, name string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, ok := f.Backends[name]; !ok {
		return fmt.Errorf("no backend %q in %s", name, path)
	}
	store, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	if store.Get(name) == nil {
		return fmt.Errorf("no stored grant for backend %q — nothing to log out", name)
	}
	if err := store.Delete(name); err != nil {
		return err
	}
	fmt.Fprintf(w, "logged out %s — grant removed from %s\n", name, store.Path())
	return nil
}

// Serve connects all configured backends, resolves the approved set, and
// serves /mcp until ctx is canceled.
func Serve(ctx context.Context, path string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	if f.Token == "" {
		return fmt.Errorf("config %s has no token — run toolhost init or set \"token\"", path)
	}

	sink, err := audit.Open(f.AuditLog)
	if err != nil {
		return err
	}
	defer sink.Close()

	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	ups, errs := ConnectBackends(ctx, f, tokens, sink, slog.Default())
	defer closeAll(ups)
	for name, e := range errs {
		fmt.Fprintf(w, "backend %s unreachable: %s\n", name, e)
	}

	enabledSet, _ := f.EnabledSet()
	res, err := core.Resolve(ups, f.ApprovedSet(), enabledSet)
	if err != nil {
		return err
	}
	for _, s := range res.Skipped {
		sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: s.Backend, Tool: s.Tool, Err: "skipped: " + s.Reason})
		fmt.Fprintf(w, "skipped %s__%s: %s\n", s.Backend, s.Tool, s.Reason)
	}

	srv, err := frontdoor.New(res, f.Token, sink)
	if err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: f.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(w, "toolhost %s serving %d approved tools on http://%s/mcp (audit: %s)\n",
		core.Version, len(res.Tools), f.Listen, f.AuditLog)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ConnectBackends dials every configured backend. Failures are per-backend:
// the gateway keeps serving the rest, and the failure is evidenced — a dead
// backend's tools are absent, hence uncallable, which IS the closed posture.
func ConnectBackends(ctx context.Context, f *config.File, tokens *oauth.Store, sink core.AuditSink, log *slog.Logger) ([]core.Upstream, map[string]error) {
	if log == nil {
		log = slog.Default()
	}
	var ups []core.Upstream
	errs := map[string]error{}
	opts := &upstream.Options{Tokens: tokens, Out: io.Discard}
	for _, name := range sortedKeys(f.Backends) {
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		up, err := upstream.Connect(connectCtx, name, f.Backends[name], opts)
		cancel()
		if err != nil {
			errs[name] = err
			sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: name, Err: err.Error()})
			log.Warn("backend unavailable", "backend", name, "err", err)
			continue
		}
		ups = append(ups, up)
	}
	return ups, errs
}

func closeAll(ups []core.Upstream) {
	for _, up := range ups {
		_ = up.Close()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
