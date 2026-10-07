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
	"net/http"
	"os"
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
	"github.com/gettoolhost/toolhost-local/internal/oauth"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
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
			qualified, err := core.Qualify(up, t.Name)
			if err != nil {
				continue
			}
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
	fmt.Fprintln(w, "live within ~2s if toolhost serve is running")
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
		token: f.Token, listen: f.Listen, auditLog: f.AuditLog,
		mode: f.Mode,
	}
	for _, up := range ups {
		live.ups[up.Namespace()] = up
	}
	for name, cfg := range f.Backends {
		live.cfgs[name] = cfg
	}

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
		case <-tick.C:
		}
		fi, err := os.Stat(l.path)
		if err != nil || (fi.ModTime() == mod && fi.Size() == size) {
			continue
		}
		mod, size = fi.ModTime(), fi.Size()
		l.mu.Lock()
		l.reload(ctx)
		l.mu.Unlock()
	}
}

// reload re-reads the config, re-dials only changed backends, and swaps the
// served surface. Callers hold l.mu.
func (l *liveSet) reload(ctx context.Context) {
	log := slog.Default()
	f, err := config.Load(l.path)
	if err != nil {
		log.Warn("reload: invalid config — keeping current surface", "err", err)
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload, Err: err.Error()})
		return
	}
	if f.Token != l.token || f.Listen != l.listen || f.AuditLog != l.auditLog || f.Mode != l.mode {
		fmt.Fprintln(l.w, "reload: listen/token/audit_log/mode changes take effect on restart — keeping current values")
	}
	// call_timeout is hot-reloadable — it applies per call, not per session.
	l.opts.DefaultCallTimeout = callTimeout(f)

	var ups []core.Upstream
	for _, name := range sortedKeys(f.Backends) {
		cfg := f.Backends[name]
		if up, ok := l.ups[name]; ok && reflect.DeepEqual(l.cfgs[name], cfg) {
			ups = append(ups, up)
			continue
		}
		if old, ok := l.ups[name]; ok {
			_ = old.Close()
			delete(l.ups, name)
		}
		connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
		up, err := upstream.Connect(connectCtx, name, cfg, l.opts)
		cancel()
		if err != nil {
			log.Warn("reload: backend unavailable", "backend", name, "err", err)
			l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: name, Err: "reload: " + err.Error()})
		} else {
			l.ups[name] = up
			ups = append(ups, up)
		}
		l.cfgs[name] = cfg
	}
	for name, up := range l.ups {
		if _, ok := f.Backends[name]; !ok {
			_ = up.Close()
			delete(l.ups, name)
			delete(l.cfgs, name)
		}
	}

	enabledSet, _ := f.EnabledSet()
	res, err := core.Resolve(ups, f.ApprovedSet(), enabledSet)
	if err != nil {
		log.Warn("reload: resolve failed — keeping current surface", "err", err)
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload, Err: err.Error()})
		return
	}
	for _, s := range res.Skipped {
		l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventBackendError, Backend: s.Backend, Tool: s.Tool, Err: "skipped: " + s.Reason})
	}
	l.fd.Reload(res)
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventReload})
	fmt.Fprintf(l.w, "reloaded: %s serving\n", toolCount(len(res.Tools)))
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
			out = append(out, core.ToolInfo{
				Qualified:   q,
				Description: t.Description,
				Approved:    approved[q],
				Enabled:     approved[q] && (!hasEn || enSet[q]),
				Requested:   requested[q],
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
func (l *liveSet) metaEnable(ctx context.Context, names []string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := config.Load(l.path)
	if err != nil {
		return nil, err
	}
	changed, err := f.Enable(names...)
	if err != nil {
		return nil, err
	}
	if err := f.Save(l.path); err != nil {
		return nil, err
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventGovern, Tool: "enable:" + strings.Join(names, ",")})
	l.reload(ctx)
	return changed, nil
}

func (l *liveSet) metaDisable(ctx context.Context, names []string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := config.Load(l.path)
	if err != nil {
		return nil, err
	}
	changed, err := f.Disable(names...)
	if err != nil {
		return nil, err
	}
	if err := f.Save(l.path); err != nil {
		return nil, err
	}
	l.sink.Record(core.Event{TS: time.Now(), Kind: core.EventGovern, Tool: "disable:" + strings.Join(names, ",")})
	l.reload(ctx)
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
	for _, info := range l.catalog() {
		if info.Approved {
			approved++
		}
		if info.Enabled {
			enabled++
		}
	}
	return map[string]any{
		"mode":      l.mode,
		"listen":    l.listen,
		"backends":  backends,
		"approved":  approved,
		"enabled":   enabled,
		"requested": f.Requested,
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
// the last 1 MiB of the file is enough history for local introspection.
func auditTail(path string, limit int, kind string) ([]core.Event, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
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
	if err := sc.Err(); err != nil {
		return nil, err
	}

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

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
