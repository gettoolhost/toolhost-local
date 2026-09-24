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
	"reflect"
	"sort"
	"strings"
	"sync"
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

	live := &liveSet{
		path:  path,
		sink:  sink,
		w:     w,
		ups:   map[string]core.Upstream{},
		cfgs:  map[string]*config.Backend{},
		opts:  &upstream.Options{Tokens: tokens, Out: io.Discard},
		token: f.Token, listen: f.Listen, auditLog: f.AuditLog,
	}
	for _, up := range ups {
		live.ups[up.Namespace()] = up
	}
	for name, cfg := range f.Backends {
		live.cfgs[name] = cfg
	}

	srv, err := frontdoor.New(res, f.Token, sink, live.meta())
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

	fmt.Fprintf(w, "toolhost %s serving %d approved tools on http://%s/mcp (audit: %s, watching config)\n",
		core.Version, len(res.Tools), f.Listen, f.AuditLog)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
	// listen address, the bearer token, or the audit path is refused.
	token, listen, auditLog string
}

// watch polls the config file and hot-applies changes: unchanged backend
// sessions are kept, added/changed/removed backends are re-dialed or
// closed, and the tool surface is swapped in place — connected clients get
// tools/list_changed. An unreadable or invalid config keeps the current
// surface; the failure is logged and audited. Saving config via any
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
	if f.Token != l.token || f.Listen != l.listen || f.AuditLog != l.auditLog {
		fmt.Fprintln(l.w, "reload: listen/token/audit_log changes take effect on restart — keeping current values")
	}

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
	fmt.Fprintf(l.w, "reloaded: %d tools serving\n", len(res.Tools))
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
		Status:  l.metaStatus,
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
