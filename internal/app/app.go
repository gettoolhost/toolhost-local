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
	ups, errs := ConnectBackends(ctx, f, audit.Discard{}, slog.Default())
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
			if f.IsApproved(qualified) {
				status = "✓"
			}
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Fprintf(w, "%-2s %-40s %s\n", status, qualified, desc)
		}
	}
	fmt.Fprintln(w, "\n✓ = approved. Approve with: toolhost approve <qualified-name>")
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

	ups, errs := ConnectBackends(ctx, f, sink, slog.Default())
	defer closeAll(ups)
	for name, e := range errs {
		fmt.Fprintf(w, "backend %s unreachable: %s\n", name, e)
	}

	res, err := core.Resolve(ups, f.ApprovedSet())
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
func ConnectBackends(ctx context.Context, f *config.File, sink core.AuditSink, log *slog.Logger) ([]core.Upstream, map[string]error) {
	if log == nil {
		log = slog.Default()
	}
	var ups []core.Upstream
	errs := map[string]error{}
	for _, name := range sortedKeys(f.Backends) {
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		up, err := upstream.Connect(connectCtx, name, f.Backends[name])
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
