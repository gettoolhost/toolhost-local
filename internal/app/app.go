// Package app wires the ports to the adapters: the use cases behind each
// CLI command. Nothing here knows HTTP flags or argv — cmd/toolhost owns
// that edge.
package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/audit"
	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
	"github.com/gettoolhost/toolhost-local/internal/namespace"
	"github.com/gettoolhost/toolhost-local/internal/oauth"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
)

// connectTimeout bounds each backend's connect+discover handshake.
const connectTimeout = 30 * time.Second

// Reconnect backoff for dead backends: 2s doubling to a 5m cap, ±25%
// jitter. A dead backend is a missing surface, not an error storm — retry
// patiently, audit every attempt, never hammer.
const (
	retryBase = 2 * time.Second
	retryCap  = 5 * time.Minute
)

// Init writes a fresh config with a generated bearer token. It refuses to
// overwrite — the token is a credential, and clobbering it silently is how
// "my gateway stopped working" bugs are born.
func Init(path string, w io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (refusing to overwrite — delete it first if you mean it)", path)
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config dir %s: %w", dir, err)
		}
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

	approved := f.ApprovedSet()
	for _, up := range ups {
		for _, t := range up.Tools() {
			qualified, err := core.Qualify(up, t.Name)
			if err != nil {
				continue
			}
			status := " "
			drifted := approved[qualified] && f.ApprovedSchemas[qualified] != "" &&
				f.ApprovedSchemas[qualified] != core.SchemaHash(t)
			switch {
			case drifted:
				status = "!"
			case f.IsEnabled(qualified):
				status = "✓"
			case approved[qualified]:
				status = "○" // approved but disabled — policy kept, surface off
			}
			desc := t.Description
			if len(desc) > 60 {
				desc = desc[:57] + "..."
			}
			fmt.Fprintf(w, "%-2s %-40s %s\n", status, qualified, desc)
		}
	}
	fmt.Fprintln(w, "\n✓ = enabled · ○ = approved but disabled · ! = schema drifted (re-approve) · blank = discovered only")
	fmt.Fprintln(w, "toolhost approve/enable/disable <qualified-name>")
	return nil
}

// EditApprovals adds (approve=true) or removes qualified names from config.
// Approve is strict: it dials the owning backends and pins each tool's
// schema hash, so the approval binds to the contract actually reviewed —
// an upstream that silently changes a schema drops the tool off the
// surface until re-approved. A name no backend currently exposes fails the
// whole call (nothing half-approved).
func EditApprovals(ctx context.Context, path string, names []string, approve bool, w io.Writer) error {
	if len(names) == 0 {
		return errors.New("name at least one qualified tool, e.g. toolhost approve fs__read_file")
	}
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	var changed []string
	if approve {
		pins, err := schemaPins(ctx, f, path, names)
		if err != nil {
			return err
		}
		if changed, err = f.Approve(pins); err != nil {
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
	fmt.Fprintln(w, "live within ~2s if toolhost serve is running")
	return nil
}

// schemaPins resolves each qualified name to the SchemaHash of the tool
// that currently exposes it — approval binds to this contract. Each name's
// candidate owners: the backend matching its namespace, plus passthrough
// backends (whose tools arrive already qualified). Names nobody can expose
// right now fail closed.
func schemaPins(ctx context.Context, f *config.File, path string, names []string) (map[string]string, error) {
	// backend name → qualified names it might expose
	want := map[string][]string{}
	for _, n := range names {
		ns, _, err := namespace.Split(n)
		if err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		found := false
		for bn, b := range f.Backends {
			if b.Passthrough || bn == ns {
				want[bn] = append(want[bn], n)
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("no backend could expose %q — check backends in config", n)
		}
	}

	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return nil, err
	}
	opts := &upstream.Options{Tokens: tokens, Out: io.Discard}
	pins := map[string]string{}
	asked := map[string]bool{}
	for _, n := range names {
		asked[n] = true
	}
	var unreachable, ambiguous []string
	for _, bn := range sortedKeys(want) {
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		up, err := upstream.Connect(connectCtx, bn, f.Backends[bn], opts)
		cancel()
		if err != nil {
			unreachable = append(unreachable, bn)
			continue
		}
		for _, t := range up.Tools() {
			q, err := core.Qualify(up, t.Name)
			if err != nil || !asked[q] {
				// Only the requested names — approving fs__read_file must
				// not approve fs__write_file alongside it.
				continue
			}
			h := core.SchemaHash(t)
			if prev, dup := pins[q]; dup && prev != h {
				// Two candidates disagree on the schema — ambiguity
				// denies; never pin an arbitrary winner.
				ambiguous = append(ambiguous, q)
				continue
			}
			pins[q] = h
		}
		_ = up.Close()
	}
	if len(ambiguous) > 0 {
		return nil, fmt.Errorf("ambiguous owner for %s — several backends expose the name with different schemas", strings.Join(ambiguous, ", "))
	}
	var missing []string
	for _, n := range names {
		if _, ok := pins[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		hint := "not currently exposed — run toolhost discover"
		if len(unreachable) > 0 {
			hint = fmt.Sprintf("backend unreachable: %s", strings.Join(unreachable, ", "))
		}
		return nil, fmt.Errorf("cannot approve %s — %s", strings.Join(missing, ", "), hint)
	}
	return pins, nil
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
	fmt.Fprintln(w, "live within ~2s if toolhost serve is running")
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
	rb, err := b.Resolved()
	if err != nil {
		return err
	}
	h, err := oauth.NewHandler(ctx, name, rb.Auth, store, true, nil, w)
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

// bearerTransport is the downstream-auth leg: the config's token on every
// request — the same header an agent's MCP client sends.
type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(clone)
}

// Status reports gateway health. Live path: one tools/call to
// toolhost__status over the real endpoint (works in both serving modes —
// the SDK negotiates). If the gateway isn't running, it still prints what
// the config knows: backends, counts, pending requests, stored grants.
func Status(ctx context.Context, path string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}

	token, err := f.ResolvedToken()
	if err != nil {
		return err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "toolhost-cli", Version: core.Version}, nil)
	connCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	session, err := client.Connect(connCtx, &mcp.StreamableClientTransport{
		Endpoint:   "http://" + f.Listen + "/mcp",
		HTTPClient: &http.Client{Transport: bearerTransport{token}},
	}, nil)
	cancel()
	if err != nil {
		fmt.Fprintf(w, "gateway: DOWN — %s/mcp not reachable\n\n", f.Listen)
		printConfigStatus(f, path, w)
		return nil
	}
	defer session.Close()

	callCtx, cancel2 := context.WithTimeout(ctx, 10*time.Second)
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: "toolhost__status"})
	cancel2()
	if err != nil {
		return fmt.Errorf("status call: %w", err)
	}
	if len(res.Content) == 0 {
		return fmt.Errorf("status call: empty response")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return fmt.Errorf("status call: unexpected content")
	}
	var report struct {
		Mode      string `json:"mode"`
		Listen    string `json:"listen"`
		Approved  int    `json:"approved"`
		Enabled   int    `json:"enabled"`
		Requested []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"requested"`
		Drifted  []string `json:"drifted"`
		Backends map[string]struct {
			Transport string `json:"transport"`
			Tools     int    `json:"tools"`
			Up        bool   `json:"up"`
		} `json:"backends"`
	}
	if err := json.Unmarshal([]byte(text.Text), &report); err != nil {
		return fmt.Errorf("status call: parse: %w", err)
	}

	fmt.Fprintf(w, "gateway: UP — http://%s/mcp (%s mode)\n", report.Listen, report.Mode)
	fmt.Fprintf(w, "tools:   %d enabled / %d approved\n", report.Enabled, report.Approved)
	for _, name := range sortedKeys(report.Backends) {
		b := report.Backends[name]
		state := "down"
		if b.Up {
			state = "up"
		}
		fmt.Fprintf(w, "backend  %-12s %-6s %s, %d tools\n", name, state, b.Transport, b.Tools)
	}
	if len(report.Requested) > 0 {
		fmt.Fprintln(w, "pending agent requests:")
		for _, r := range report.Requested {
			fmt.Fprintf(w, "  %s — %s\n", r.Name, r.Reason)
		}
	}
	if len(report.Drifted) > 0 {
		fmt.Fprintln(w, "schema drifted — re-approve to pin the new contract:")
		for _, n := range report.Drifted {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
	return nil
}

// printConfigStatus is the down-gateway view — everything knowable without
// a live process: configured backends, gate counts, pending requests, and
// which oauth backends hold stored grants.
func printConfigStatus(f *config.File, path string, w io.Writer) {
	fmt.Fprintf(w, "config:  %s (mode %s)\n", path, f.Mode)
	for _, name := range sortedKeys(f.Backends) {
		b := f.Backends[name]
		fmt.Fprintf(w, "backend  %-12s configured  %s\n", name, b.Transport)
	}
	en, has := f.EnabledSet()
	if has {
		fmt.Fprintf(w, "tools:   %d enabled / %d approved\n", len(en), len(f.ApprovedSet()))
	} else {
		fmt.Fprintf(w, "tools:   %d approved (all enabled — no allowlist)\n", len(f.ApprovedSet()))
	}
	for _, r := range f.Requested {
		fmt.Fprintf(w, "requested %s — %s\n", r.Name, r.Reason)
	}
	if store, err := oauth.OpenStore(oauth.StorePath(path)); err == nil {
		for _, name := range sortedKeys(f.Backends) {
			if f.Backends[name].Auth != nil && f.Backends[name].Auth.Type == "oauth" {
				if store.Get(name) != nil {
					fmt.Fprintf(w, "grant    %-12s stored\n", name)
				} else {
					fmt.Fprintf(w, "grant    %-12s MISSING — run: toolhost auth %s\n", name, name)
				}
			}
		}
	}
}

// Serve connects all configured backends, resolves the approved set, and
// serves until ctx is canceled. stdio=false: HTTP /mcp with bearer. stdio=
// true: MCP over stdin/stdout — the whole gateway attachable to any client
// that spawns subprocesses; no bearer (the spawning process owns the pipe),
// status lines must go to w=stderr (stdout is the protocol).
func Serve(ctx context.Context, path string, w io.Writer, stdio bool) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	token, err := f.ResolvedToken()
	if err != nil {
		return err
	}
	if !stdio && token == "" {
		return fmt.Errorf("config %s has no token — run toolhost init or set \"token\"", path)
	}

	// A relative audit_log anchors to the config file's directory — the
	// canonical home only works if companions live beside the config no
	// matter where serve runs. (The File keeps the literal value so a
	// load/save cycle never bakes an absolute path into the config.)
	auditLog := f.AuditLog
	if !filepath.IsAbs(auditLog) {
		auditLog = filepath.Join(filepath.Dir(path), auditLog)
	}
	// Create the log's directory — a relative audit_log under a fresh
	// config dir must not fail the boot.
	if err := os.MkdirAll(filepath.Dir(auditLog), 0o700); err != nil {
		return fmt.Errorf("audit log dir: %w", err)
	}
	sink, err := audit.Open(auditLog, int64(f.AuditMaxMB)<<20)
	if err != nil {
		return err
	}
	defer sink.Close()

	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	ups, errs := ConnectBackends(ctx, f, tokens, sink, slog.Default())
	for name, e := range errs {
		fmt.Fprintf(w, "backend %s unreachable: %s\n", name, e)
	}

	enabledSet, _ := f.EnabledSet()
	res, err := core.Resolve(ups, f.ApprovedSet(), enabledSet, f.ApprovedSchemas)
	if err != nil {
		closeAll(ups) // no orphaned stdio subprocesses on a bad config
		return err
	}
	for _, d := range res.Drifted {
		sink.Record(core.Event{TS: time.Now(), Kind: core.EventSchemaDrift, Backend: d.Backend, Tool: d.Tool})
		fmt.Fprintf(w, "schema drift: %s dropped — re-approve to pin the new contract\n", d.Qualified)
	}
	for _, s := range res.Skipped {
		sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: s.Backend, Tool: s.Tool, Err: "skipped: " + s.Reason})
		fmt.Fprintf(w, "skipped %s__%s: %s\n", s.Backend, s.Tool, s.Reason)
	}
	if len(f.Requested) > 0 {
		fmt.Fprintf(w, "%d pending agent request(s) — review with: toolhost status\n", len(f.Requested))
	}

	live := &liveSet{
		path:  path,
		sink:  sink,
		w:     w,
		ups:   map[string]core.Upstream{},
		cfgs:  map[string]*config.Backend{},
		opts:  &upstream.Options{Tokens: tokens, Out: io.Discard, DefaultCallTimeout: callTimeout(f)},
		token: f.Token, listen: f.Listen, auditLog: auditLog,
		mode:        f.Mode,
		ctx:         ctx,
		dead:        make(chan deadSig, 8),
		deadPending: map[string]deadSig{},
		retry:       map[string]*retryState{},
		dial: func(ctx context.Context, name string, cfg *config.Backend, opts *upstream.Options) (core.Upstream, error) {
			return upstream.Connect(ctx, name, cfg, opts)
		},
	}
	for _, up := range ups {
		live.ups[up.Namespace()] = up
		live.watchSession(up.Namespace(), up)
	}
	for name, cfg := range f.Backends {
		live.cfgs[name] = cfg
	}
	for name := range errs {
		live.markRetry(name)
	}
	// Shutdown closes the LIVE pool, not the boot slice — sessions replaced
	// by reconnects would otherwise leak their transport and watcher.
	defer live.closeAll()

	if stdio {
		srv := frontdoor.NewBare(res, sink, live.meta())
		live.fd = srv
		go live.watch(ctx)
		fmt.Fprintf(w, "toolhost %s serving %s over stdio (audit: %s, watching config)\n",
			core.Version, toolCount(len(res.Tools)), f.AuditLog)
		return srv.ServeStdio(ctx)
	}

	srv, err := frontdoor.New(res, token, sink, live.meta(),
		&frontdoor.Options{Stateless: f.Mode != config.ModeStateful})
	if err != nil {
		return err
	}
	live.fd = srv
	go live.watch(ctx)

	httpSrv := &http.Server{Addr: f.Listen, Handler: srv.Handler()}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	fmt.Fprintf(w, "toolhost %s serving %s on http://%s/mcp (audit: %s, watching config)\n",
		core.Version, toolCount(len(res.Tools)), f.Listen, f.AuditLog)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func toolCount(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", n)
}

// liveSet is the currently-served backend pool: sessions keyed by namespace
// plus the config that produced each, so a reload keeps unchanged sessions
// and re-dials only what changed. mu serializes the watch goroutine and the
// toolhost__* meta-tool handlers — both mutate the pool.
type liveSet struct {
	mu   sync.Mutex
	path string
	fd   *frontdoor.Server
	sink core.AuditSink
	w    io.Writer

	ups  map[string]core.Upstream
	cfgs map[string]*config.Backend
	opts *upstream.Options

	// Restart-required fields from the boot config — hot-changing the
	// listen address, the bearer token, the audit path, or the serving
	// mode is refused.
	token, listen, auditLog string
	mode                    string

	// ctx is the gateway-lifetime context — session watchers and reconnect
	// dials live as long as Serve does. Request contexts (which die with
	// the HTTP call that spawned them) must never reach a session watcher.
	ctx context.Context

	// Reconnect machinery: dead carries session-death signals from
	// per-backend watcher goroutines; deadPending parks signals that
	// arrive while the channel is full so none are lost; retry is the
	// per-backend backoff. dial is injectable for tests — production
	// wires upstream.Connect.
	dead        chan deadSig
	deadPending map[string]deadSig
	retry       map[string]*retryState
	dial        func(ctx context.Context, name string, cfg *config.Backend, opts *upstream.Options) (core.Upstream, error)
	// closed is set by closeAll on Serve exit — a dial completing after
	// shutdown must not install a session nobody will close.
	closed bool
}

// deadSig is one session-death signal. The up pointer is the identity
// check: a session we deliberately closed or replaced must not ghost-kill
// its successor.
type deadSig struct {
	name string
	up   core.Upstream
	err  error
}

// retryState is one backend's reconnect backoff.
type retryState struct {
	attempts int
	nextAt   time.Time
}

// watch polls the config file and hot-applies changes: unchanged backend
// sessions are kept, added/changed/removed backends are re-dialed or
// closed, and the tool surface is swapped in place — subscribed modern
// clients and legacy stateful sessions get tools/list_changed. An unreadable
// or invalid config keeps the current surface; the failure is logged and
// audited. Saving config via any
// toolhost command is atomic, so a torn file is only ever a hand-edit mid-
// write — the next tick retries.
func (l *liveSet) watch(ctx context.Context) {
	var mod time.Time
	var size int64
	if fi, err := os.Stat(l.path); err == nil {
		mod, size = fi.ModTime(), fi.Size()
	}
	tick := time.NewTicker(1500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-l.dead:
			l.mu.Lock()
			if l.noteDead(sig) {
				l.republish()
			}
			l.mu.Unlock()
			continue
		case <-tick.C:
		}
		fi, err := os.Stat(l.path)
		if err != nil || (fi.ModTime() == mod && fi.Size() == size) {
			l.retrySweep()
			continue
		}
		mod, size = fi.ModTime(), fi.Size()
		l.reload()
	}
}

// reload re-reads the config, re-dials only changed backends, and swaps the
// served surface. Three phases mirroring retrySweep: snapshot under l.mu,
// dial WITHOUT it (a slow backend must never stall the meta-tools), commit
// under the lock with a config-equality check so a newer reload wins.
// Everything runs on the gateway context — a request that triggers reload
// must not carry its lifetime into watchers or dials.
func (l *liveSet) reload() {
	ctx := l.ctx
	log := slog.Default()
	f, err := config.Load(l.path)
	if err != nil {
		log.Warn("reload: invalid config — keeping current surface", "err", err)
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload, Err: err.Error()})
		return
	}

	l.mu.Lock()
	reloadAudit := f.AuditLog
	if !filepath.IsAbs(reloadAudit) {
		reloadAudit = filepath.Join(filepath.Dir(l.path), reloadAudit)
	}
	if f.Token != l.token || f.Listen != l.listen || reloadAudit != l.auditLog || f.Mode != l.mode {
		fmt.Fprintln(l.w, "reload: listen/token/audit_log/mode changes take effect on restart — keeping current values")
	}
	// call_timeout is hot-reloadable — it applies per call, not per session.
	l.opts.DefaultCallTimeout = callTimeout(f)

	// Phase 1: compute the dial set and evict the dead/removed under lock.
	toDial := map[string]*config.Backend{}
	for _, name := range sortedKeys(f.Backends) {
		cfg := f.Backends[name]
		if cur, ok := l.cfgs[name]; ok && reflect.DeepEqual(cur, cfg) {
			_, alive := l.ups[name]
			_, pending := l.retry[name]
			if alive || pending {
				continue // unchanged config — the session or its backoff owns it
			}
			// unchanged but dead with nothing scheduled — dial now
		}
		if old, ok := l.ups[name]; ok {
			_ = old.Close()
			delete(l.ups, name)
		}
		delete(l.retry, name) // config changed — fresh backoff for the new shape
		toDial[name] = cfg
		l.cfgs[name] = cfg // intent: the mid-dial commit check compares against this
	}
	for name, up := range l.ups {
		if _, ok := f.Backends[name]; !ok {
			_ = up.Close()
			delete(l.ups, name)
		}
	}
	// A backend absent from the new config must leave NO trace — l.cfgs is
	// what a mid-flight retry dial compares against, and l.retry is what
	// schedules one. Dead backends sit in neither l.ups nor (after this) the
	// retry set, so a stale cfgs entry would let a removed backend resurrect.
	for name := range l.cfgs {
		if _, ok := f.Backends[name]; !ok {
			delete(l.cfgs, name)
		}
	}
	for name := range l.retry {
		if _, ok := f.Backends[name]; !ok {
			delete(l.retry, name)
		}
	}
	// Publish now — unconditionally: policy edits (enable/disable/approve)
	// change the surface without touching backends, and evicted backends
	// leave immediately rather than haunting it for the dial window.
	if n := l.publish(f); n >= 0 {
		fmt.Fprintf(l.w, "reloaded: %s serving\n", toolCount(n))
	}
	l.mu.Unlock()

	// Phase 2: dial outside the lock — in parallel, so a slow backend can't
	// serialize its 30s timeout across every sibling.
	type result struct {
		name string
		cfg  *config.Backend
		up   core.Upstream
		err  error
	}
	var results []result
	var resMu sync.Mutex
	var wg sync.WaitGroup
	for name, cfg := range toDial {
		wg.Add(1)
		go func(name string, cfg *config.Backend) {
			defer wg.Done()
			connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			up, err := l.dial(connectCtx, name, cfg, l.opts)
			cancel()
			resMu.Lock()
			results = append(results, result{name, cfg, up, err})
			resMu.Unlock()
		}(name, cfg)
	}
	wg.Wait()

	// Phase 3: commit under lock — a reload that landed mid-dial wins.
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for _, r := range results {
		if r.err != nil {
			log.Warn("reload: backend unavailable", "backend", r.name, "err", r.err)
			l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: r.name, Err: "reload: " + r.err.Error()})
			l.markRetry(r.name)
			changed = true
			continue
		}
		if _, alive := l.ups[r.name]; alive || l.closed || !reflect.DeepEqual(l.cfgs[r.name], r.cfg) {
			_ = r.up.Close()
			continue
		}
		l.ups[r.name] = r.up
		l.watchSession(r.name, r.up)
		changed = true
	}
	if changed {
		l.republish()
	}
}

// publish is the shared tail of reload and reconnect: resolve the live
// pool against f's policy, evidence skips and schema drift, swap the front
// door. Returns the served tool count, or -1 when a failed resolve keeps
// the current surface. Callers hold l.mu.
func (l *liveSet) publish(f *config.File) int {
	ups := make([]core.Upstream, 0, len(l.ups))
	for _, name := range sortedKeys(l.ups) {
		ups = append(ups, l.ups[name])
	}
	enabledSet, _ := f.EnabledSet()
	res, err := core.Resolve(ups, f.ApprovedSet(), enabledSet, f.ApprovedSchemas)
	if err != nil {
		slog.Default().Warn("resolve failed — keeping current surface", "err", err)
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload, Err: err.Error()})
		return -1
	}
	for _, s := range res.Skipped {
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: s.Backend, Tool: s.Tool, Err: "skipped: " + s.Reason})
	}
	for _, d := range res.Drifted {
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventSchemaDrift, Backend: d.Backend, Tool: d.Tool})
		fmt.Fprintf(l.w, "schema drift: %s dropped — re-approve to pin the new contract\n", d.Qualified)
	}
	l.fd.Reload(res)
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload})
	return len(res.Tools)
}

// republish re-resolves against the config on disk — the path a reconnect
// or a session death takes when no config change drove them. Callers hold
// l.mu.
func (l *liveSet) republish() {
	f, err := config.Load(l.path)
	if err != nil {
		slog.Default().Warn("republish: invalid config — keeping current surface", "err", err)
		return
	}
	l.publish(f)
}

// noteDead drops a backend whose session ended. Identity-checked: a
// deliberately closed or already-replaced session reports nothing. Returns
// true when the pool changed. Callers hold l.mu.
func (l *liveSet) noteDead(sig deadSig) bool {
	up, ok := l.ups[sig.name]
	if !ok || up != sig.up {
		return false
	}
	delete(l.ups, sig.name)
	l.markRetry(sig.name)
	errText := "session ended — will reconnect"
	if sig.err != nil {
		errText = "session ended: " + sig.err.Error()
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: sig.name, Err: errText})
	fmt.Fprintf(l.w, "backend %s lost — reconnecting with backoff\n", sig.name)
	return true
}

// drainDead applies every queued session-death signal. Callers hold l.mu.
func (l *liveSet) drainDead() bool {
	changed := false
	for {
		select {
		case sig := <-l.dead:
			changed = l.noteDead(sig) || changed
		default:
			for name, sig := range l.deadPending {
				changed = l.noteDead(sig) || changed
				delete(l.deadPending, name)
			}
			return changed
		}
	}
}

// markRetry records one more failed/dropped generation for a backend and
// schedules the next attempt with exponential backoff + jitter.
// Callers hold l.mu.
func (l *liveSet) markRetry(name string) {
	st := l.retry[name]
	if st == nil {
		st = &retryState{}
		l.retry[name] = st
	}
	st.attempts++
	delay := retryBase << min(st.attempts-1, 8)
	if delay > retryCap {
		delay = retryCap
	}
	delay += time.Duration(mrand.Int63n(int64(delay)/4 + 1))
	st.nextAt = time.Now().Add(delay)
}

// dueRetries returns the configured-and-dead backends whose backoff has
// elapsed. Stale entries (alive or unconfigured) are swept. Callers hold
// l.mu.
func (l *liveSet) dueRetries() map[string]*config.Backend {
	now := time.Now()
	due := map[string]*config.Backend{}
	for name, st := range l.retry {
		if _, alive := l.ups[name]; alive {
			delete(l.retry, name)
			continue
		}
		cfg, ok := l.cfgs[name]
		if !ok || st.nextAt.After(now) {
			if !ok {
				delete(l.retry, name)
			}
			continue
		}
		due[name] = cfg
	}
	return due
}

// retrySweep re-dials due backends. Connects run WITHOUT l.mu, in
// parallel — a slow backend must never stall the meta-tools or serialize
// its timeout across siblings; results commit under the lock with a
// config-equality check so a reload mid-dial wins. Death signals queued
// during the dial are drained before publishing.
func (l *liveSet) retrySweep() {
	ctx := l.ctx
	l.mu.Lock()
	dropped := l.drainDead()
	if dropped {
		// Dead backends leave the surface NOW — don't let ghost tools haunt
		// the front door for the duration of the redials.
		l.republish()
	}
	due := l.dueRetries()
	l.mu.Unlock()

	type result struct {
		name string
		up   core.Upstream
		err  error
	}
	var results []result
	var resMu sync.Mutex
	var wg sync.WaitGroup
	for name, cfg := range due {
		wg.Add(1)
		go func(name string, cfg *config.Backend) {
			defer wg.Done()
			connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
			up, err := l.dial(connectCtx, name, cfg, l.opts)
			cancel()
			resMu.Lock()
			results = append(results, result{name, up, err})
			resMu.Unlock()
		}(name, cfg)
	}
	wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	changed := dropped
	for _, r := range results {
		if r.err != nil {
			l.markRetry(r.name)
			l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: r.name, Err: "reconnect: " + r.err.Error()})
			continue
		}
		// A reload mid-dial, a duplicate win, or gateway shutdown takes
		// precedence — the fresher party owns the slot; this session
		// closes unclaimed.
		if _, alive := l.ups[r.name]; alive || l.closed || !reflect.DeepEqual(l.cfgs[r.name], due[r.name]) {
			_ = r.up.Close()
			continue
		}
		l.ups[r.name] = r.up
		delete(l.retry, r.name)
		l.watchSession(r.name, r.up)
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendUp, Backend: r.name})
		fmt.Fprintf(l.w, "backend %s reconnected\n", r.name)
		changed = true
	}
	changed = l.drainDead() || changed
	if changed {
		l.republish()
	}
}

// signalDeath queues a session-death signal for the watch loop. The send
// is non-blocking — Call handlers must never stall on reconnect latency
// while the watch loop sits inside a dial. A signal that can't queue is
// parked in deadPending (keyed by backend, latest wins) rather than
// dropped: the Wait-driven signal is one-shot, and losing it would leave
// a quiet dead backend stranded until a call happened to error.
func (l *liveSet) signalDeath(sig deadSig) {
	select {
	case l.dead <- sig:
	default:
		l.mu.Lock()
		l.deadPending[sig.name] = sig
		l.mu.Unlock()
	}
}

// reportCallError turns a transport-level Call failure into the same death
// signal a Wait() watcher produces. HTTP sessions can stay logically alive
// after the wire dies, so liveness can't rely on Wait() alone. Caller-side
// context errors — timeouts, client cancellation — are NOT death: a slow
// call must not bounce a healthy backend.
func (l *liveSet) reportCallError(up core.Upstream, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	l.mu.Lock()
	cur, ok := l.ups[up.Namespace()]
	l.mu.Unlock()
	if !ok || cur != up {
		return // already replaced — a stale session's error kills nothing
	}
	l.signalDeath(deadSig{name: up.Namespace(), up: up, err: err})
}

// watchSession turns a session's natural end into a reconnect request.
// Upstreams that can't report liveness simply don't get the watcher —
// they still reconnect on config reload.
func (l *liveSet) watchSession(name string, up core.Upstream) {
	w, ok := up.(interface{ Wait() error })
	if !ok {
		return
	}
	go func() {
		err := w.Wait()
		l.signalDeath(deadSig{name: name, up: up, err: err})
	}()
}

// meta exposes the liveSet as the gateway's own control plane — the
// toolhost__* tools an agent calls to discover and govern its surface at
// runtime. Enable/Disable write through to the config file, so an agent's
// choices persist exactly like the CLI's.
func (l *liveSet) meta() *frontdoor.Meta {
	return &frontdoor.Meta{
		Search:  l.metaSearch,
		List:    l.metaList,
		Enable:  l.metaEnable,
		Disable: l.metaDisable,
		Request: l.metaRequest,
		Status:  l.metaStatus,
		Report:  l.metaReport,
		Audit:   l.metaAudit,
		CallErr: l.reportCallError,
	}
}

// catalog is every discovered tool with its governance state. Callers hold
// l.mu.
func (l *liveSet) catalog() []core.ToolInfo {
	f, err := config.Load(l.path)
	if err != nil {
		return nil
	}
	approved := f.ApprovedSet()
	enSet, hasEn := f.EnabledSet()
	requested := f.RequestedSet()
	var out []core.ToolInfo
	for _, ns := range sortedKeys(l.ups) {
		for _, t := range l.ups[ns].Tools() {
			q, err := core.Qualify(l.ups[ns], t.Name)
			if err != nil {
				continue
			}
			pin := f.ApprovedSchemas[q]
			drifted := approved[q] && pin != "" && pin != core.SchemaHash(t)
			out = append(out, core.ToolInfo{
				Qualified:   q,
				Description: t.Description,
				Approved:    approved[q],
				Enabled:     approved[q] && !drifted && (!hasEn || enSet[q]),
				Requested:   requested[q],
				Drifted:     drifted,
			})
		}
	}
	return out
}

func (l *liveSet) metaSearch(_ context.Context, query string) ([]core.ToolInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	query = strings.ToLower(query)
	var hits []core.ToolInfo
	for _, info := range l.catalog() {
		if query == "" || strings.Contains(strings.ToLower(info.Qualified), query) ||
			strings.Contains(strings.ToLower(info.Description), query) {
			hits = append(hits, info)
		}
	}
	return hits, nil
}

func (l *liveSet) metaList(_ context.Context, scope string) ([]core.ToolInfo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []core.ToolInfo
	for _, info := range l.catalog() {
		switch scope {
		case "", "enabled":
			if info.Enabled {
				out = append(out, info)
			}
		case "approved":
			if info.Approved {
				out = append(out, info)
			}
		case "all":
			out = append(out, info)
		default:
			return nil, fmt.Errorf("scope must be \"enabled\", \"approved\" or \"all\", got %q", scope)
		}
	}
	return out, nil
}

// metaEnable is the agent-facing enable: same gate as the CLI (approved
// only), same file, then an immediate reload so the surface reflects it.
// l.mu serializes the load→save pair against other config writers; the
// reload runs AFTER unlock — it manages its own locking and holding it
// here would deadlock.
func (l *liveSet) metaEnable(ctx context.Context, names []string) ([]string, error) {
	l.mu.Lock()
	f, err := config.Load(l.path)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	changed, err := f.Enable(names...)
	if err == nil {
		err = f.Save(l.path)
	}
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventGovern, Tool: "enable:" + strings.Join(names, ",")})
	l.mu.Unlock()
	l.reload()
	return changed, nil
}

func (l *liveSet) metaDisable(ctx context.Context, names []string) ([]string, error) {
	l.mu.Lock()
	f, err := config.Load(l.path)
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	changed, err := f.Disable(names...)
	if err == nil {
		err = f.Save(l.path)
	}
	if err != nil {
		l.mu.Unlock()
		return nil, err
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventGovern, Tool: "disable:" + strings.Join(names, ",")})
	l.mu.Unlock()
	l.reload()
	return changed, nil
}

// metaRequest is the agent's formal ask: queue qualified names in the
// config's `requested` list. It writes the same file the CLI edits — the
// watcher picks it up — but a request never widens the surface; only a
// human `toolhost approve` does.
func (l *liveSet) metaRequest(_ context.Context, names []string, reason string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := config.Load(l.path)
	if err != nil {
		return nil, err
	}
	queued, err := f.RequestTools(reason, names...)
	if err != nil {
		return nil, err
	}
	if err := f.Save(l.path); err != nil {
		return nil, err
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventGovern, Tool: "request:" + strings.Join(names, ",")})
	return queued, nil
}

// metaReport is the gateway's live health — the data `toolhost status`
// prints and toolhost__status returns. Callers hold no lock here; it takes
// its own.
func (l *liveSet) metaReport() any {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := config.Load(l.path)
	if err != nil {
		f = &config.File{}
	}

	type backendState struct {
		Transport string `json:"transport"`
		Tools     int    `json:"tools"`
		Up        bool   `json:"up"`
	}
	backends := map[string]backendState{}
	for name, cfg := range l.cfgs {
		b := backendState{Transport: cfg.Transport}
		if up, ok := l.ups[name]; ok {
			b.Up = true
			b.Tools = len(up.Tools())
		}
		backends[name] = b
	}
	var approved, enabled int
	var drifted []string
	for _, info := range l.catalog() {
		if info.Approved {
			approved++
		}
		if info.Enabled {
			enabled++
		}
		if info.Drifted {
			drifted = append(drifted, info.Qualified)
		}
	}
	return map[string]any{
		"mode":      l.mode,
		"listen":    l.listen,
		"backends":  backends,
		"approved":  approved,
		"enabled":   enabled,
		"requested": f.Requested,
		"drifted":   drifted,
	}
}

func (l *liveSet) metaStatus(name string) (core.ToolInfo, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, info := range l.catalog() {
		if info.Qualified == name {
			return info, true
		}
	}
	return core.ToolInfo{}, false
}

// metaAudit is the agent-facing read on the evidence trail: the last `limit`
// events, optionally one kind. Agents get introspection without file access;
// the log never carried arguments or secrets, so it is safe to expose.
func (l *liveSet) metaAudit(_ context.Context, limit int, kind string) ([]core.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	return auditTail(l.auditLog, limit, kind)
}

// auditTail returns the last `limit` events from a JSONL audit log in
// chronological order, filtered to `kind` when set. A bounded tail-read:
// the last 1 MiB of each segment is enough history for local
// introspection — after a rotation the previous generation (<path>.1)
// backfills whatever the fresh live segment doesn't cover.
func auditTail(path string, limit int, kind string) ([]core.Event, error) {
	var lines []string
	prev, _ := tailLines(path + ".1") // rotated generation — best effort
	lines = append(lines, prev...)
	cur, err := tailLines(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		// Live segment deleted but the rotated .1 remains — return its
		// history rather than erroring out of toolhost__audit.
		if len(lines) == 0 {
			return nil, nil
		}
	}
	lines = append(lines, cur...)

	events := make([]core.Event, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(events) < limit; i-- {
		var ev core.Event
		if json.Unmarshal([]byte(lines[i]), &ev) != nil {
			continue
		}
		if kind != "" && ev.Kind != kind {
			continue
		}
		events = append(events, ev)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events, nil
}

// tailLines returns the complete lines in the last 1 MiB of a file — the
// first scanned line is torn by the mid-record seek and dropped.
func tailLines(path string) ([]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()

	const tailWindow = 1 << 20
	fi, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if fi.Size() > tailWindow {
		start = fi.Size() - tailWindow
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if start > 0 {
		sc.Scan() // the first line is torn — drop it
	}
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
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
	opts := &upstream.Options{Tokens: tokens, Out: io.Discard, DefaultCallTimeout: callTimeout(f)}
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

// callTimeout parses the configured bound; config validation already ran,
// so an unparsable value can't arrive — the fallback is the documented
// default anyway.
func callTimeout(f *config.File) time.Duration {
	if d, err := time.ParseDuration(f.CallTimeout); err == nil && d > 0 {
		return d
	}
	d, _ := time.ParseDuration(config.DefaultCallTimeout)
	return d
}

func closeAll(ups []core.Upstream) {
	for _, up := range ups {
		_ = up.Close()
	}
}

// closeAll shuts down every session currently holding a slot — including
// ones reconnected after boot, which the initial dial slice never saw.
func (l *liveSet) closeAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	for name, up := range l.ups {
		_ = up.Close()
		delete(l.ups, name)
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
