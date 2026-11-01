// mcpcli.go — the `toolhost mcp` server registry: the federated
// `X mcp add` grammar pointed at toolhost.json. The developer's habit
// stays; the binary swaps.
package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/oauth"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
)

// MCP dispatches the registry group: `toolhost mcp <sub> [flags] [args]`.
// args arrives as raw argv — mcp's grammar is "flags anywhere, command
// tail literal", which is not stdlib-flag-shaped, so parsing lives here.
func MCP(ctx context.Context, argv []string, w io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: toolhost mcp <add|list|get|remove|enable|disable|test|tools|auth|logout|import>")
	}
	sub, rest := argv[0], argv[1:]
	switch sub {
	case "add":
		return MCPAdd(ctx, rest, w)
	case "list":
		return MCPList(rest, w)
	case "get":
		return MCPGet(rest, w)
	case "remove":
		return MCPRemove(rest, w)
	case "enable", "disable":
		return MCPSetEnabled(rest, sub == "enable", w)
	case "test":
		return MCPTest(ctx, rest, w)
	case "tools":
		return MCPTools(ctx, rest, w)
	case "auth":
		return oneName(rest, w, func(path, name string) error {
			return Auth(ctx, path, name, w)
		})
	case "logout":
		return oneName(rest, w, func(path, name string) error {
			return Logout(path, name, w)
		})
	case "import":
		return MCPImport(rest, w)
	default:
		return fmt.Errorf("unknown mcp command %q — add|list|get|remove|enable|disable|test|tools|auth|logout|import", sub)
	}
}

// oneName is the shared `-c` + one positional shape (auth, logout).
func oneName(argv []string, w io.Writer, run func(path, name string) error) error {
	cfg, rest, err := parseC(argv)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: toolhost mcp %s [-c config] <name>", argv[0])
	}
	return run(config.DefaultPath(cfg), rest[0])
}

// ── mcp add ────────────────────────────────────────────────────────────

// addArgs is the parsed `mcp add` invocation: flags may appear anywhere;
// everything positional after the name is the literal command tail.
type addArgs struct {
	cfg          string
	name         string
	tail         []string // url or command+args — the positional tail
	transport    string
	env          []string // KEY=val repeatable
	envAllowlist string
	envInherit   bool
	cwd          string
	headers      []string // "K: v" repeatable
	token        string   // env:VAR
	auth         string
	clientID     string
	clientSecret string
	scopes       string
	passthrough  bool
	approveAll   bool
	approve      string // comma names
	callTimeout  string
	disabled     bool
	rawJSON      string // "" = none, "-" = stdin
	fromClient   string
	scope        string
	dryRun       bool
}

// knownValueFlags are mcp add's value-taking flags — everything else in
// argv that looks flag-like lands in the positional tail.
var addValueFlags = map[string]func(*addArgs, string) error{
	"-t":              func(a *addArgs, v string) error { a.transport = v; return nil },
	"--transport":     func(a *addArgs, v string) error { a.transport = v; return nil },
	"-e":              func(a *addArgs, v string) error { a.env = append(a.env, v); return nil },
	"--env":           func(a *addArgs, v string) error { a.env = append(a.env, v); return nil },
	"-H":              func(a *addArgs, v string) error { a.headers = append(a.headers, v); return nil },
	"--header":        func(a *addArgs, v string) error { a.headers = append(a.headers, v); return nil },
	"--url":           func(a *addArgs, v string) error { a.tail = append(a.tail, v); return nil },
	"--token":         func(a *addArgs, v string) error { a.token = v; return nil },
	"--auth":          func(a *addArgs, v string) error { a.auth = v; return nil },
	"--client-id":     func(a *addArgs, v string) error { a.clientID = v; return nil },
	"--client-secret": func(a *addArgs, v string) error { a.clientSecret = v; return nil },
	"--scopes":        func(a *addArgs, v string) error { a.scopes = v; return nil },
	"--approve":       func(a *addArgs, v string) error { a.approve = v; return nil },
	"--env-allowlist": func(a *addArgs, v string) error { a.envAllowlist = v; return nil },
	"--cwd":           func(a *addArgs, v string) error { a.cwd = v; return nil },
	"--from":          func(a *addArgs, v string) error { a.fromClient = v; return nil },
	"--json":          func(a *addArgs, v string) error { a.rawJSON = v; return nil },
	"-s":              func(a *addArgs, v string) error { a.scope = v; return nil },
	"--scope":         func(a *addArgs, v string) error { a.scope = v; return nil },
	"-c":              func(a *addArgs, v string) error { a.cfg = v; return nil },
	"--call-timeout":  func(a *addArgs, v string) error { a.callTimeout = v; return nil },
}

var addBoolFlags = map[string]func(*addArgs) error{
	"--passthrough": func(a *addArgs) error { a.passthrough = true; return nil },
	"--approve-all": func(a *addArgs) error { a.approveAll = true; return nil },
	"--disabled":    func(a *addArgs) error { a.disabled = true; return nil },
	"--env-inherit": func(a *addArgs) error { a.envInherit = true; return nil },
	"--dry-run":     func(a *addArgs) error { a.dryRun = true; return nil },
}

// parseAdd splits argv into known flags and the positional tail. A lone
// `--` makes the rest literal — the escape for a server command whose own
// args collide with our flags.
func parseAdd(argv []string) (*addArgs, error) {
	a := &addArgs{}
	var positional []string
	literal := false
	for i := 0; i < len(argv); i++ {
		tok := argv[i]
		if literal {
			positional = append(positional, tok)
			continue
		}
		if tok == "--" {
			literal = true
			continue
		}
		if fn, ok := addValueFlags[tok]; ok {
			if i+1 >= len(argv) {
				return nil, fmt.Errorf("%s needs a value", tok)
			}
			if err := fn(a, argv[i+1]); err != nil {
				return nil, err
			}
			i++
			continue
		}
		// --flag=value form
		if strings.HasPrefix(tok, "--") {
			if k, v, ok := strings.Cut(tok, "="); ok {
				if fn, known := addValueFlags[k]; known {
					if err := fn(a, v); err != nil {
						return nil, err
					}
					continue
				}
			}
			if fn, ok := addBoolFlags[tok]; ok {
				if err := fn(a); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("unknown flag %s", tok)
		}
		if strings.HasPrefix(tok, "-") && len(tok) == 2 {
			if fn, ok := addBoolFlags[tok]; ok {
				if err := fn(a); err != nil {
					return nil, err
				}
				continue
			}
		}
		positional = append(positional, tok)
	}
	if len(positional) > 0 {
		a.name = positional[0]
		a.tail = append(a.tail, positional[1:]...)
	}
	return a, nil
}

// MCPAdd registers a backend. Write-only by default — approval flags dial
// to pin schemas, but the entry lands regardless.
func MCPAdd(ctx context.Context, argv []string, w io.Writer) error {
	a, err := parseAdd(argv)
	if err != nil {
		return err
	}
	path := config.DefaultPath(a.cfg)
	if a.scope != "" {
		var err error
		if path, err = scopedPath(a.scope); err != nil {
			return err
		}
	}
	if a.name == "" {
		return addWizard(ctx, a, path, w)
	}
	b, err := a.backend()
	if err != nil {
		return err
	}
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, dup := f.Backends[a.name]; dup {
		return fmt.Errorf("backend %q already exists — toolhost mcp remove %s first", a.name, a.name)
	}
	if err := f.SetBackend(a.name, b); err != nil {
		return err
	}

	if a.dryRun {
		raw, _ := json.MarshalIndent(b, "", "  ")
		fmt.Fprintf(w, "would write backends.%s:\n%s\n", a.name, raw)
		return nil
	}
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "added backend %s (%s)\n", a.name, b.Transport)
	warnIfMissingCommand(w, b)

	// Approval intents dial to pin — the grant is a separate step that
	// may fail without undoing the add.
	switch {
	case a.approveAll:
		return approveWhole(ctx, path, a.name, w)
	case a.approve != "":
		names := splitComma(a.approve)
		return EditApprovals(ctx, path, names, true, false, w)
	}
	return nil
}

// backend builds the config entry from the parsed add — positional tail,
// --json form, or --from-client extraction.
func (a *addArgs) backend() (*config.Backend, error) {
	switch {
	case a.rawJSON != "":
		raw := a.rawJSON
		if raw == "-" {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return nil, fmt.Errorf("--json -: read stdin: %w", err)
			}
			raw = string(data)
		}
		return entryToBackend([]byte(raw))
	case a.fromClient != "":
		return fromClientEntry(a.fromClient, a.name)
	}
	return a.tailBackend()
}

// tailBackend maps the positional tail onto a Backend: a URL means http,
// bare words mean stdio command+args.
func (a *addArgs) tailBackend() (*config.Backend, error) {
	b := &config.Backend{
		Passthrough: a.passthrough,
		Disabled:    a.disabled,
		Cwd:         a.cwd,
		EnvInherit:  a.envInherit,
		CallTimeout: a.callTimeout,
	}
	if len(a.tail) == 0 {
		return nil, fmt.Errorf("usage: toolhost mcp add <name> <command…|https://url>  (or --json / --from)")
	}
	b.Transport = a.transport
	isURL := strings.HasPrefix(a.tail[0], "http://") || strings.HasPrefix(a.tail[0], "https://")
	switch {
	case isURL || b.Transport == "http" || b.Transport == "sse":
		if b.Transport == "" {
			b.Transport = "http"
		}
		b.URL = a.tail[0]
		if len(a.tail) > 1 {
			return nil, fmt.Errorf("unexpected arguments after url: %s", strings.Join(a.tail[1:], " "))
		}
		headers, err := parseHeaders(a.headers)
		if err != nil {
			return nil, err
		}
		b.Headers = headers
		auth, err := a.authBlock()
		if err != nil {
			return nil, err
		}
		b.Auth = auth
	default:
		b.Transport = "stdio"
		if b.Transport = a.transportOr("stdio"); b.Transport != "stdio" {
			return nil, fmt.Errorf("transport %q with no url — add the url or -t http", a.transport)
		}
		b.Command = a.tail[0]
		b.Args = a.tail[1:]
	}
	env, err := parseEnv(a.env)
	if err != nil {
		return nil, err
	}
	b.Env = env
	if a.envAllowlist != "" {
		b.EnvAllowlist = splitComma(a.envAllowlist)
	}
	return b, nil
}

func (a *addArgs) transportOr(def string) string {
	if a.transport != "" {
		return a.transport
	}
	return def
}

// authBlock assembles the upstream-auth leg for an http backend.
func (a *addArgs) authBlock() (*config.Auth, error) {
	if a.token != "" {
		if !strings.HasPrefix(a.token, "env:") {
			fmt.Fprintf(os.Stderr, "note: --token wants an env: reference (--token env:VAR) so the secret stays out of the file\n")
		}
		return &config.Auth{Type: "bearer", Token: a.token}, nil
	}
	switch a.auth {
	case "", "none":
		return nil, nil
	case "oauth":
		return &config.Auth{Type: "oauth", ClientID: a.clientID, ClientSecret: a.clientSecret}, nil
	case "cc", "client_credentials":
		return &config.Auth{Type: "client_credentials",
			ClientID: a.clientID, ClientSecret: a.clientSecret, Scopes: splitComma(a.scopes)}, nil
	}
	return nil, fmt.Errorf("--auth must be none|bearer|oauth|cc, got %q", a.auth)
}

// scopedPath maps -s onto an env file — user | project | shared.
func scopedPath(scope string) (string, error) {
	switch scope {
	case "user":
		return config.ConfigHome() + "/toolhost.json", nil
	case "project":
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return config.ProjectConfigPath(cwd), nil
	case "shared":
		return config.SharedConfigPath, nil
	}
	return "", fmt.Errorf("--scope must be user|project|shared, got %q", scope)
}

// approveWhole pins every tool the backend currently exposes — the
// --approve-all grant: it dials once, right at add time.
func approveWhole(ctx context.Context, path, name string, w io.Writer) error {
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[name]
	if !ok {
		return fmt.Errorf("backend %q not found", name)
	}
	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	up, err := upstream.Connect(connectCtx, name, b, &upstream.Options{Tokens: tokens, Out: io.Discard})
	cancel()
	if err != nil {
		fmt.Fprintf(w, "backend %s written but unreachable — approve later: %v\n", name, err)
		return nil
	}
	defer up.Close()

	pins := map[string]string{}
	var skipped []string
	for _, t := range up.Tools() {
		q, err := core.Qualify(up, t.Name)
		if err != nil {
			skipped = append(skipped, t.Name)
			continue
		}
		pins[q] = core.SchemaHash(t)
	}
	changed, err := f.Approve(pins)
	if err != nil {
		return err
	}
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "approved %d tools on %s\n", len(changed), name)
	for _, s := range skipped {
		fmt.Fprintf(w, "  skipped %s — cannot be namespaced\n", s)
	}
	return nil
}

// warnIfMissingCommand notes when a stdio command isn't on PATH — a
// warning, not a refusal: PATH at serve time may differ.
func warnIfMissingCommand(w io.Writer, b *config.Backend) {
	if b.Transport != "stdio" {
		return
	}
	if _, err := exec.LookPath(b.Command); err != nil {
		fmt.Fprintf(w, "note: %q is not on PATH — the backend will fail until it is\n", b.Command)
	}
}

// addWizard is the bare `mcp add` on a terminal — asks in words instead
// of demanding grammar.
func addWizard(ctx context.Context, a *addArgs, path string, w io.Writer) error {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("usage: toolhost mcp add <name> <command…|https://url>")
	}
	r := bufio.NewReader(os.Stdin)
	ask := func(prompt string) string {
		fmt.Fprint(w, prompt)
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line)
	}
	name := ask("name: ")
	if name == "" {
		return fmt.Errorf("name is required")
	}
	a.name = name
	kind := ask("type — command or url? ")
	spec := ask("command or url: ")
	switch {
	case strings.HasPrefix(spec, "http"):
		a.transport = "http"
	case spec == "":
		return fmt.Errorf("a command or url is required")
	default:
		a.transport = "stdio"
	}
	if kind != "" && !strings.HasPrefix(kind, "c") {
		a.transport = "http"
	}
	a.tail = []string{spec}
	if a.transport == "stdio" {
		for {
			e := ask("env KEY=val (blank to finish): ")
			if e == "" {
				break
			}
			a.env = append(a.env, e)
		}
	}
	if strings.HasPrefix(ask("approve all its tools? [y/N] "), "y") {
		a.approveAll = true
	}
	b, err := a.tailBackend()
	if err != nil {
		return err
	}
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, dup := f.Backends[name]; dup {
		return fmt.Errorf("backend %q already exists", name)
	}
	if err := f.SetBackend(name, b); err != nil {
		return err
	}
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "added backend %s (%s)\n", name, b.Transport)
	warnIfMissingCommand(w, b)
	if a.approveAll {
		return approveWhole(ctx, path, name, w)
	}
	return nil
}

// ── mcp list / get / remove / enable / disable ─────────────────────────

func MCPList(argv []string, w io.Writer) error {
	cfg, rest, asJSON, err := parseCJSON(argv)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("usage: toolhost mcp list [-c config] [--json]")
	}
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	type row struct {
		Name      string `json:"name"`
		Transport string `json:"transport"`
		Disabled  bool   `json:"disabled,omitempty"`
		Approved  int    `json:"approved"`
	}
	var rows []row
	for _, name := range sortedKeys(f.Backends) {
		b := f.Backends[name]
		rows = append(rows, row{Name: name, Transport: b.Transport,
			Disabled: b.Disabled, Approved: len(f.BackendToolsApproved(name))})
	}
	if asJSON {
		return jsonOut(w, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "no backends — toolhost mcp add <name> <command…|url>")
		return nil
	}
	fmt.Fprintf(w, "%-16s %-8s %-10s %s\n", "name", "transport", "state", "approved tools")
	for _, r := range rows {
		state := "ready"
		if r.Disabled {
			state = "parked"
		}
		fmt.Fprintf(w, "%-16s %-8s %-10s %d\n", r.Name, r.Transport, state, r.Approved)
	}
	return nil
}

func MCPGet(argv []string, w io.Writer) error {
	cfg, rest, asJSON, err := parseCJSON(argv)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: toolhost mcp get [-c config] <name> [--json]")
	}
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[rest[0]]
	if !ok {
		return fmt.Errorf("no backend %q — toolhost mcp list", rest[0])
	}
	if asJSON {
		return jsonOut(w, b)
	}
	raw, _ := json.MarshalIndent(b, "", "  ")
	fmt.Fprintf(w, "backend %s:\n%s\n", rest[0], raw)
	if tools := f.BackendToolsApproved(rest[0]); len(tools) > 0 {
		fmt.Fprintf(w, "approved: %s\n", strings.Join(tools, ", "))
	}
	if b.Auth != nil && b.Auth.Type == "oauth" {
		store, err := oauth.OpenStore(oauth.StorePath(path))
		if err == nil {
			if store.Get(rest[0]) != nil {
				fmt.Fprintln(w, "grant:   stored")
			} else {
				fmt.Fprintf(w, "grant:   MISSING — run: toolhost mcp auth %s\n", rest[0])
			}
		}
	}
	return nil
}

func MCPRemove(argv []string, w io.Writer) error {
	var cfg, name string
	var prune, dryRun bool
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "-c":
			i++
			if i >= len(argv) {
				return fmt.Errorf("-c needs a value")
			}
			cfg = argv[i]
		case "--prune-approvals":
			prune = true
		case "--dry-run":
			dryRun = true
		default:
			name = argv[i]
		}
	}
	if name == "" {
		return fmt.Errorf("usage: toolhost mcp remove [-c config] <name> [--prune-approvals] [--dry-run]")
	}
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	if _, ok := f.Backends[name]; !ok {
		return fmt.Errorf("no backend %q", name)
	}
	held := f.BackendToolsApproved(name)
	if len(held) > 0 && !prune {
		return fmt.Errorf("backend %q holds %d approvals (%s…) — removing it would delete policy silently.\n"+
			"  to prune approvals with it: toolhost mcp remove %s --prune-approvals\n"+
			"  to park it instead:           toolhost mcp disable %s",
			name, len(held), held[0], name, name)
	}
	if dryRun {
		fmt.Fprintf(w, "would remove backend %s", name)
		if len(held) > 0 {
			fmt.Fprintf(w, " + %d approvals (%s)", len(held), strings.Join(held, ", "))
		}
		fmt.Fprintln(w)
		return nil
	}
	var pruned []string
	if prune {
		pruned = f.PruneBackendTools(name)
	}
	f.RemoveBackend(name)
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed backend %s\n", name)
	for _, q := range pruned {
		fmt.Fprintf(w, "  pruned %s\n", q)
	}
	fmt.Fprintln(w, "live within ~2s if toolhost serve is running")
	return nil
}

func MCPSetEnabled(argv []string, enable bool, w io.Writer) error {
	cfg, rest, err := parseC(argv)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: toolhost mcp %s [-c config] <name>", map[bool]string{true: "enable", false: "disable"}[enable])
	}
	name := rest[0]
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[name]
	if !ok {
		return fmt.Errorf("no backend %q", name)
	}
	if b.Disabled == !enable {
		fmt.Fprintf(w, "backend %s already %s\n", name, map[bool]string{true: "enabled", false: "parked"}[enable])
		return nil
	}
	b.Disabled = !enable
	if err := f.Save(path); err != nil {
		return err
	}
	if enable {
		fmt.Fprintf(w, "enabled backend %s — reconnecting on next serve tick\n", name)
	} else {
		fmt.Fprintf(w, "parked backend %s — its tools left the surface\n", name)
	}
	return nil
}

// ── mcp test / tools — the introspection pair ──────────────────────────

func MCPTest(ctx context.Context, argv []string, w io.Writer) error {
	cfg, rest, asJSON, err := parseCJSON(argv)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: toolhost mcp test [-c config] <name> [--json]")
	}
	name := rest[0]
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[name]
	if !ok {
		return fmt.Errorf("no backend %q", name)
	}
	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	start := time.Now()
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	up, err := upstream.Connect(connectCtx, name, b, &upstream.Options{Tokens: tokens, Out: io.Discard})
	cancel()
	ms := time.Since(start).Milliseconds()
	if err != nil {
		if asJSON {
			return jsonOut(w, map[string]any{"backend": name, "ok": false, "error": err.Error(), "ms": ms})
		}
		fmt.Fprintf(w, "%-16s FAIL  %v\n", name, err)
		return fmt.Errorf("backend %s unreachable", name)
	}
	defer up.Close()
	if asJSON {
		return jsonOut(w, map[string]any{"backend": name, "ok": true, "transport": b.Transport,
			"tools": len(up.Tools()), "ms": ms})
	}
	fmt.Fprintf(w, "%-16s OK    %s, %d tools, %dms\n", name, b.Transport, len(up.Tools()), ms)
	if b.Auth != nil && b.Auth.Type == "oauth" {
		if tokens.Get(name) == nil {
			fmt.Fprintf(w, "  grant: MISSING — run: toolhost mcp auth %s\n", name)
		} else {
			fmt.Fprintln(w, "  grant: stored")
		}
	}
	return nil
}

func MCPTools(ctx context.Context, argv []string, w io.Writer) error {
	cfg, rest, asJSON, err := parseCJSON(argv)
	if err != nil {
		return err
	}
	var namesOnly bool
	var rest2 []string
	for _, t := range rest {
		if t == "--names-only" {
			namesOnly = true
		} else {
			rest2 = append(rest2, t)
		}
	}
	if len(rest2) != 1 {
		return fmt.Errorf("usage: toolhost mcp tools [-c config] <name> [--names-only] [--json]")
	}
	name := rest2[0]
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	b, ok := f.Backends[name]
	if !ok {
		return fmt.Errorf("no backend %q", name)
	}
	tokens, err := oauth.OpenStore(oauth.StorePath(path))
	if err != nil {
		return err
	}
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	up, err := upstream.Connect(connectCtx, name, b, &upstream.Options{Tokens: tokens, Out: io.Discard})
	cancel()
	if err != nil {
		return fmt.Errorf("backend %s unreachable: %w", name, err)
	}
	defer up.Close()
	tools := up.Tools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	if asJSON {
		return jsonOut(w, tools)
	}
	for _, t := range tools {
		q, _ := core.Qualify(up, t.Name)
		if namesOnly {
			fmt.Fprintln(w, q)
			continue
		}
		desc := t.Description
		if len(desc) > 70 {
			desc = desc[:67] + "..."
		}
		fmt.Fprintf(w, "%-42s %s\n", q, desc)
	}
	return nil
}

// ── mcp import — absorb a client's server entries ──────────────────────

func MCPImport(argv []string, w io.Writer) error {
	var cfg, client string
	var dryRun bool
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "-c":
			i++
			if i >= len(argv) {
				return fmt.Errorf("-c needs a value")
			}
			cfg = argv[i]
		case "--dry-run":
			dryRun = true
		default:
			client = argv[i]
		}
	}
	if client == "" {
		return fmt.Errorf("usage: toolhost mcp import [-c config] <client> [--dry-run]")
	}
	entries, err := clientServerEntries(client)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("no server entries found in %s's config", client)
	}
	path := config.DefaultPath(cfg)
	f, err := config.Load(path)
	if err != nil {
		return err
	}
	var imported, skipped []string
	for name, entry := range entries {
		if name == "local-gateway" || name == "toolhost" || name == "gateway" {
			skipped = append(skipped, name+" (ours)")
			continue
		}
		if _, dup := f.Backends[name]; dup {
			skipped = append(skipped, name+" (already a backend)")
			continue
		}
		b, err := entryToBackend(entry)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		if err := f.SetBackend(name, b); err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		imported = append(imported, name)
		if dryRun {
			raw, _ := json.MarshalIndent(b, "", "  ")
			fmt.Fprintf(w, "would import %s:\n%s\n", name, raw)
		}
	}
	if dryRun {
		fmt.Fprintf(w, "dry-run: %d would import, %d skipped\n", len(imported), len(skipped))
		for _, s := range skipped {
			fmt.Fprintf(w, "  skipped %s\n", s)
		}
		return nil
	}
	if err := f.Save(path); err != nil {
		return err
	}
	fmt.Fprintf(w, "imported %d backend(s) from %s: %s\n", len(imported), client, strings.Join(imported, ", "))
	for _, s := range skipped {
		fmt.Fprintf(w, "  skipped %s\n", s)
	}
	if len(imported) > 0 {
		fmt.Fprintln(w, "their entries still live in the client config — converge to one server: toolhost attach "+client)
	}
	return nil
}

// entryToBackend normalizes any client's server-entry JSON into our
// Backend shape — the inverse of attach's per-client writers.
func entryToBackend(raw []byte) (*config.Backend, error) {
	var e map[string]any
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("entry is not JSON: %w", err)
	}
	b := &config.Backend{}

	// url fields across dialects
	for _, k := range []string{"url", "serverUrl", "httpUrl"} {
		if v, ok := e[k].(string); ok && v != "" {
			b.URL = v
			break
		}
	}
	if b.URL != "" {
		b.Transport = "http"
		if h, ok := e["headers"].(map[string]any); ok {
			b.Headers = map[string]string{}
			for k, v := range h {
				if s, ok := v.(string); ok {
					b.Headers[k] = s
				}
			}
		}
		return b, nil
	}

	// stdio: command is a string everywhere except opencode's argv array
	// and zed's bare string.
	var args []string
	if arr, ok := e["command"].([]any); ok && len(arr) > 0 {
		if s, ok := arr[0].(string); ok {
			b.Command = s
		}
		for _, v := range arr[1:] {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	} else if s, ok := e["command"].(string); ok && s != "" {
		b.Command = s
	}
	if arr, ok := e["args"].([]any); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				args = append(args, s)
			}
		}
	}
	if b.Command == "" {
		return nil, fmt.Errorf("entry has neither url nor command — nothing to import")
	}
	b.Transport = "stdio"
	b.Args = args
	if env, ok := e["env"].(map[string]any); ok && len(env) > 0 {
		b.Env = map[string]string{}
		for k, v := range env {
			if s, ok := v.(string); ok {
				b.Env[k] = s
			}
		}
	}
	return b, nil
}

// clientServerEntries reads a client's server map — same target table
// attach writes to, read back as name → raw entry JSON.
func clientServerEntries(client string) (map[string]json.RawMessage, error) {
	t, ok := attachTargets[client]
	if !ok {
		return nil, fmt.Errorf("unknown client %q — known: %s", client, attachTargetNames())
	}
	if t.cli != nil || t.path == nil {
		return nil, fmt.Errorf("%s manages its own config — ask it (e.g. `codex mcp get <name>`) and paste: toolhost mcp add %s --json -", client, "<name>")
	}
	path, err := t.path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var servers map[string]json.RawMessage
	if block, ok := doc[t.topKey]; ok {
		if err := json.Unmarshal(block, &servers); err != nil {
			return nil, fmt.Errorf("%s[%q] is not a server map: %w", path, t.topKey, err)
		}
	}
	// claude merges under the top-level too
	if len(servers) == 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
		delete(servers, t.topKey)
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("%s has no %q entries", path, t.topKey)
	}
	// fromClientEntry below needs the raw doc too — keep it simple:
	// entries are already normalized name → entry JSON.
	return servers, nil
}

// fromClientEntry extracts one named server from a client's config —
// `mcp add <name> --from <client>`.
func fromClientEntry(client, name string) (*config.Backend, error) {
	entries, err := clientServerEntries(client)
	if err != nil {
		return nil, err
	}
	raw, ok := entries[name]
	if !ok {
		var names []string
		for n := range entries {
			names = append(names, n)
		}
		return nil, fmt.Errorf("no server %q in %s's config — has: %s", name, client, strings.Join(names, ", "))
	}
	return entryToBackend(raw)
}

// ── small shared parsers ───────────────────────────────────────────────

// parseC pulls just -c out of argv — for commands whose remaining args
// are plain positionals.
func parseC(argv []string) (cfg string, rest []string, err error) {
	for i := 0; i < len(argv); i++ {
		if argv[i] == "-c" {
			if i+1 >= len(argv) {
				return "", nil, fmt.Errorf("-c needs a value")
			}
			cfg = argv[i+1]
			i++
			continue
		}
		rest = append(rest, argv[i])
	}
	return cfg, rest, nil
}

// parseCJSON pulls -c and --json — the read-verb flag pair.
func parseCJSON(argv []string) (cfg string, rest []string, asJSON bool, err error) {
	cfg, rest, err = parseC(argv)
	var kept []string
	for _, t := range rest {
		if t == "--json" {
			asJSON = true
		} else {
			kept = append(kept, t)
		}
	}
	return cfg, kept, asJSON, err
}

func jsonOut(w io.Writer, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(raw))
	return err
}

func parseEnv(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("env wants KEY=val, got %q", kv)
		}
		env[k] = v
	}
	return env, nil
}

func parseHeaders(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	h := map[string]string{}
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, ":")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("header wants \"K: v\", got %q", kv)
		}
		h[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return h, nil
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
