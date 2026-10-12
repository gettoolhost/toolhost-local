package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Attach registers the gateway with an MCP client — the last mile of
// install. File-backed clients get an ownership-preserving, compare-and-
// swap merge of the mcpServers map; CLI-backed clients get their `mcp add`
// invocation executed. --stdio attaches the spawn form (serve --stdio)
// instead of the HTTP endpoint + bearer; --dry-run prints the planned
// action and resulting entry without writing.
func Attach(cfgPath, client string, stdio, printOnly, dryRun bool, w io.Writer) error {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	f, err := config.Load(abs)
	if err != nil {
		return err
	}

	url := "http://" + f.Listen + "/mcp"
	token := ""
	if !stdio {
		token, err = f.ResolvedToken()
		if err != nil {
			return fmt.Errorf("attach needs the gateway token: %w", err)
		}
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}

	if printOnly || client == "json" {
		fmt.Fprintf(w, "add this server entry to your MCP client config:\n\n")
		raw, _ := json.MarshalIndent(map[string]any{"toolhost": serverEntry(url, token, bin, abs, "url", stdio)}, "  ", "  ")
		fmt.Fprintf(w, "  \"mcpServers\": %s\n", raw)
		return nil
	}

	t, ok := attachTargets[client]
	if !ok {
		return fmt.Errorf("unknown client %q — known: %s, or use --print for the JSON", client, attachTargetNames())
	}
	if t.cli != nil {
		argv := t.cli(url, token, bin, abs, stdio)
		if dryRun {
			fmt.Fprintf(w, "would run: %s\n", strings.Join(redactToken(argv, token), " "))
			return nil
		}
		// Echo with the bearer redacted — argv carries the token for exec;
		// scrollback shouldn't.
		fmt.Fprintf(w, "running: %s\n", strings.Join(redactToken(argv, token), " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Run(); err != nil {
			return err
		}
		if client == "codex" && !stdio {
			fmt.Fprintln(w, "note: codex reads the bearer from $TOOLHOST_TOKEN — export it where codex runs")
		}
		provisionSkill(t, w)
		return nil
	}
	path, err := t.path()
	if err != nil {
		return err
	}
	build := t.entry
	if build == nil {
		build = serverEntry
	}
	res, err := planMCPServerEntry(path, t.topKey, build(url, token, bin, abs, t.urlField, stdio), t.conditional)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(w, "%s %s\n", res.action, path)
		if res.action == planCreate || res.action == planUpdate {
			raw, _ := json.MarshalIndent(res.doc[t.topKey].(map[string]any)["toolhost"], "  ", "  ")
			// The entry carries the gateway bearer in headers — mask it
			// in display output exactly as the CLI branch does.
			raw = redactTokenBytes(raw, token)
			fmt.Fprintf(w, "  \"toolhost\": %s\n", raw)
		}
		return nil
	}
	switch res.action {
	case planSkip:
		fmt.Fprintf(w, "toolhost already attached in %s — nothing to do\n", path)
		provisionSkill(t, w)
		return nil
	case planAbsent:
		fmt.Fprintf(w, "%s does not exist — %s only merges into an existing config; create it (or install the client) first\n", path, t.display)
		return nil
	}
	if err := commitMCPServerEntry(res); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s toolhost server entry in %s\n", res.action.pastTense(), res.path)
	if !stdio {
		fmt.Fprintf(w, "gateway must be serving: toolhost serve (or toolhost install)\n")
	}
	provisionSkill(t, w)
	return nil
}

// AttachDetect is the no-argument form: survey every known client and
// report which are attached, which are set up but not attached, and which
// aren't installed. It touches no gateway config — detection is about the
// clients, not the endpoint — and writes nothing.
func AttachDetect(w io.Writer) error {
	fmt.Fprintln(w, "client              state")
	for _, name := range sortedKeys(attachTargets) {
		t := attachTargets[name]
		state := detectTarget(name, t)
		fmt.Fprintf(w, "%-20s%s\n", name, state)
	}
	fmt.Fprintln(w, "\nattach with: toolhost attach <client> (add --stdio for the spawn form, --dry-run to preview)")
	return nil
}

// detectTarget reports one client's attach state without writing anything.
// File targets are read (never planned — detection must not resolve the
// gateway token or touch config); CLI targets are probed by binary.
func detectTarget(name string, t attachTarget) string {
	if t.cli != nil {
		if _, err := exec.LookPath(name); err != nil {
			return "not installed"
		}
		return "installed — not attach-detectable via CLI; run: toolhost attach " + name
	}
	path, err := t.path()
	if err != nil {
		return "unsupported on this platform"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if t.conditional {
			return "not set up"
		}
		return "no config yet — attach would create " + path
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return "config present but not plain JSON — check manually: " + path
	}
	if servers, ok := doc[t.topKey].(map[string]any); ok {
		if _, has := servers["toolhost"]; has {
			return "attached — " + path
		}
	}
	return "not attached — run: toolhost attach " + name
}

// attachTarget is one MCP client's attach strategy, expressed as data.
// File targets merge the "toolhost" entry into topKey ("mcpServers", or
// "servers" for VS Code) via a compare-and-swap write; CLI targets exec
// the client's own registrar when we don't own its file format. urlField
// is the client's name for the endpoint key ("url" for most, "serverUrl"
// for windsurf, "httpUrl" for gemini). conditional targets only merge
// into a config file that already exists — an absent file means the
// client isn't set up, and creating a bare one would mislead it.
type attachTarget struct {
	display     string
	topKey      string
	urlField    string
	path        func() (string, error)
	cli         func(url, token, bin, cfg string, stdio bool) []string
	conditional bool
	// entry builds a nonstandard server entry when serverEntry's shape
	// doesn't fit the client (e.g. VS Code requires "type" on stdio).
	entry func(url, token, bin, cfg, urlField string, stdio bool) map[string]any
	// skillDir is the client's user-level skill directory — the one place
	// a SKILL.md teaching the toolhost__* control plane can live without
	// editing files the user owns. Nil: no safe provisioning point.
	skillDir func() (string, error)
}

var attachTargets = map[string]attachTarget{
	// Claude Code's user scope is ~/.claude.json — a plain mcpServers map
	// we can merge directly instead of shelling out to `claude mcp add`.
	// Conditional: it's Claude's whole config, not a dedicated MCP file —
	// an absent one means Claude isn't set up, so don't fabricate it.
	"claude": {
		display:     "Claude Code",
		topKey:      "mcpServers",
		path:        func() (string, error) { return homeJoin(".claude.json") },
		urlField:    "url",
		conditional: true,
		skillDir:    func() (string, error) { return homeJoin(".claude", "skills", "toolhost-gateway") },
	},
	"devin": {
		display:  "Devin",
		cli:      devinArgs,
		skillDir: func() (string, error) { return homeJoin(".config", "devin", "skills", "toolhost-gateway") },
	},
	"cursor": {
		display:  "Cursor",
		topKey:   "mcpServers",
		urlField: "url",
		path:     func() (string, error) { return homeJoin(".cursor", "mcp.json") },
	},
	"windsurf": {
		display:  "Windsurf",
		topKey:   "mcpServers",
		urlField: "serverUrl",
		path:     func() (string, error) { return homeJoin(".codeium", "windsurf", "mcp_config.json") },
	},
	"vscode": {
		display:  "VS Code",
		topKey:   "servers", // VS Code mcp.json namespaces under "servers"
		urlField: "url",
		path:     vscodeMCPPath,
		entry:    vscodeEntry, // VS Code's schema wants an explicit "type"
	},
	"gemini": {
		display:     "Gemini CLI",
		topKey:      "mcpServers",
		urlField:    "httpUrl", // streamable HTTP field name in gemini settings
		path:        func() (string, error) { return homeJoin(".gemini", "settings.json") },
		conditional: true, // settings.json is gemini's main config — only merge if present
	},
	// Codex keeps its servers in ~/.codex/config.toml — TOML we don't own,
	// so registration goes through `codex mcp add`. Its HTTP form takes a
	// bearer env-var NAME rather than the token literal.
	"codex": {
		display: "Codex",
		cli:     codexArgs,
	},
	"opencode": {
		display:     "OpenCode",
		topKey:      "mcp", // opencode.json namespaces servers under "mcp"
		urlField:    "url",
		path:        opencodePath,
		entry:       opencodeEntry, // "type": "local"/"remote", command is an argv array
		conditional: true,
	},
	"zed": {
		display:     "Zed",
		topKey:      "context_servers", // zed's name for the server map
		urlField:    "url",
		path:        zedPath,
		entry:       zedEntry, // command is a bare string beside args, not an argv
		conditional: true,     // settings.json is zed's whole config — merge only if present
	},
	// Cline and Roo are VS Code extensions — their MCP config lives in the
	// editor's globalStorage, which only exists once the extension does.
	"cline": {
		display:  "Cline",
		topKey:   "mcpServers",
		urlField: "url",
		path: func() (string, error) {
			return vscodeExtSettingsPath("saoudrizwan.claude-dev", "cline_mcp_settings.json")
		},
		conditional: true,
	},
	"roo": {
		display:  "Roo Code",
		topKey:   "mcpServers",
		urlField: "url",
		path: func() (string, error) {
			return vscodeExtSettingsPath("rooveterinaryinc.roo-cline", "mcp_settings.json")
		},
		conditional: true,
	},
	// Claude Desktop only speaks stdio — the entry is always the spawn
	// form regardless of --stdio; there is no remote shape to offer.
	"claude-desktop": {
		display:     "Claude Desktop",
		topKey:      "mcpServers",
		path:        claudeDesktopPath,
		entry:       claudeDesktopEntry,
		conditional: true,
	},
}

func attachTargetNames() string {
	names := make([]string, 0, len(attachTargets))
	for n := range attachTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// pastTense renders the action for the post-commit message.
func (a planAction) pastTense() string {
	switch a {
	case planCreate:
		return "created"
	case planUpdate:
		return "updated"
	default:
		return "attached"
	}
}

// vscodeEntry is serverEntry plus the explicit "type" VS Code's servers
// schema wants — "stdio" on the spawn form, "http" already covered.
func vscodeEntry(url, token, bin, cfg, urlField string, stdio bool) map[string]any {
	e := serverEntry(url, token, bin, cfg, urlField, stdio)
	if stdio {
		e["type"] = "stdio"
	}
	return e
}

// vscodeMCPPath is VS Code's per-user MCP config, which moved out of
// settings.json into a dedicated mcp.json. Honors XDG_CONFIG_HOME on
// Linux, APPDATA on Windows.
func vscodeMCPPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return homeJoin("Library", "Application Support", "Code", "User", "mcp.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA unset — cannot locate VS Code config")
		}
		return filepath.Join(appData, "Code", "User", "mcp.json"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Code", "User", "mcp.json"), nil
		}
		return homeJoin(".config", "Code", "User", "mcp.json")
	}
}

// opencodePath is OpenCode's global config — an XDG path on every
// platform it runs on.
func opencodePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.json"), nil
	}
	return homeJoin(".config", "opencode", "opencode.json")
}

// opencodeEntry is serverEntry in OpenCode's shape: "local" servers take
// the spawn as an argv array, "remote" ones a url plus headers. An absent
// "enabled" means enabled — never write a true that would clobber a
// user's manual disable on refresh.
func opencodeEntry(url, token, bin, cfg, urlField string, stdio bool) map[string]any {
	if stdio {
		return map[string]any{
			"type":    "local",
			"command": []string{bin, "serve", "--stdio", "-c", cfg},
		}
	}
	return map[string]any{
		"type":    "remote",
		urlField:  url,
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}
}

// zedPath is Zed's settings.json — XDG on Linux, ~/.config on macOS,
// APPDATA on Windows.
func zedPath() (string, error) {
	if runtime.GOOS == "windows" {
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA unset — cannot locate Zed config")
		}
		return filepath.Join(appData, "Zed", "settings.json"), nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "zed", "settings.json"), nil
	}
	return homeJoin(".config", "zed", "settings.json")
}

// zedEntry is serverEntry in Zed's context_servers shape: a remote server
// is url plus headers; a local one is a bare command STRING beside args —
// not the argv form every other client uses.
func zedEntry(url, token, bin, cfg, urlField string, stdio bool) map[string]any {
	if stdio {
		return map[string]any{
			"command": bin,
			"args":    []string{"serve", "--stdio", "-c", cfg},
		}
	}
	return map[string]any{
		urlField:  url,
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}
}

// vscodeExtSettingsPath is where a VS Code extension keeps its MCP
// settings — inside the editor's per-user globalStorage. Same layout as
// vscodeMCPPath's root, one extension dir deeper.
func vscodeExtSettingsPath(extensionID, filename string) (string, error) {
	var dir string
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, "Library", "Application Support", "Code", "User", "globalStorage")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA unset — cannot locate VS Code globalStorage")
		}
		dir = filepath.Join(appData, "Code", "User", "globalStorage")
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			dir = filepath.Join(xdg, "Code", "User", "globalStorage")
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			dir = filepath.Join(home, ".config", "Code", "User", "globalStorage")
		}
	}
	return filepath.Join(dir, extensionID, "settings", filename), nil
}

// claudeDesktopPath is the desktop app's MCP config — macOS and Windows
// only; there is no Linux build to point at.
func claudeDesktopPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return homeJoin("Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", errors.New("APPDATA unset — cannot locate Claude Desktop config")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	default:
		return "", errors.New("Claude Desktop has no Linux build — nothing to attach")
	}
}

// claudeDesktopEntry always emits the stdio spawn — the desktop app has
// no remote transport, so --stdio and the default mean the same thing.
func claudeDesktopEntry(url, token, bin, cfg, urlField string, stdio bool) map[string]any {
	return map[string]any{
		"command": bin,
		"args":    []string{"serve", "--stdio", "-c", cfg},
	}
}

// serverEntry is the server-map fragment for this gateway — either the
// HTTP endpoint with bearer, or the stdio spawn of this same binary.
func serverEntry(url, token, bin, cfg, urlField string, stdio bool) map[string]any {
	if stdio {
		return map[string]any{
			"command": bin,
			"args":    []string{"serve", "--stdio", "-c", cfg},
		}
	}
	return map[string]any{
		"type":    "http",
		urlField:  url,
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}
}

// mergePlan is the computed attach — the merged doc plus what applying it
// means. Planning and committing are separate so --dry-run prints exactly
// what a real attach would do. path is the symlink-resolved target.
type mergePlan struct {
	action  planAction
	path    string
	before  []byte // file contents at read time — the CAS compare side
	existed bool   // whether the file existed — CAS distinguishes absent from empty
	doc     map[string]any
}

type planAction string

const (
	planCreate planAction = "create"   // file new or entry absent → write
	planUpdate planAction = "update"   // existing toolhost entry refreshed
	planSkip   planAction = "attached" // already identical — no write
	planAbsent planAction = "skipped"  // conditional target, file missing
)

// planMCPServerEntry reads the client config, decides the attach action,
// and builds the merged document. Ownership rules: an identical toolhost
// entry is a no-op; an entry we recognize as ours (a stale gateway entry)
// is updated in place, preserving keys the client or user added; a
// toolhost entry we don't recognize is FOREIGN — we refuse rather than
// clobber someone's server. A conditional target with no file is skipped,
// never created.
func planMCPServerEntry(path, topKey string, entry map[string]any, conditional bool) (*mergePlan, error) {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	existed := err == nil
	switch {
	case err == nil:
		// UseNumber keeps every JSON number as its literal — a plain
		// Unmarshal turns ints > 2^53 into float64s and re-encoding
		// silently corrupts fields we never touched (nanosecond
		// timestamps, big install IDs in ~/.claude.json).
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if doc == nil {
			// A file containing the literal "null" unmarshals into a nil
			// map — treat it as an empty config, don't panic.
			doc = map[string]any{}
		}
		// Write through symlinks: renaming over a linked config would
		// replace the link itself with a plain file.
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			path = real
		}
	case errors.Is(err, os.ErrNotExist):
		if conditional {
			return &mergePlan{action: planAbsent, path: path, doc: doc}, nil
		}
		raw = nil
	default:
		return nil, err
	}
	var servers map[string]any
	if tv, present := doc[topKey]; present {
		var ok bool
		if servers, ok = tv.(map[string]any); !ok {
			return nil, fmt.Errorf("%s has a non-object %q key — refusing to rewrite it", path, topKey)
		}
	} else {
		servers = map[string]any{}
		doc[topKey] = servers
	}
	var existing map[string]any
	if ev, present := servers["toolhost"]; present {
		var ok bool
		if existing, ok = ev.(map[string]any); !ok {
			// A "toolhost" entry that isn't an object is foreign to us —
			// same refusal as an unrecognized object.
			return nil, fmt.Errorf("%s already has a \"toolhost\" server that isn't this gateway — refusing to overwrite it; remove or rename it first", path)
		}
	}
	switch {
	case existing == nil:
		servers["toolhost"] = entry
		return &mergePlan{action: planCreate, path: path, before: raw, existed: existed, doc: doc}, nil
	case sameJSON(existing, entry):
		return &mergePlan{action: planSkip, path: path, before: raw, existed: existed, doc: doc}, nil
	case !looksLikeOurs(existing):
		return nil, fmt.Errorf("%s already has a \"toolhost\" server that isn't this gateway — refusing to overwrite it; remove or rename it first", path)
	default:
		// Ours but stale — rewrite our shape, keep client-added keys.
		// Owned keys absent from the new entry are dropped first: a
		// transport switch (stdio→http) must not leave `command`/`args`
		// beside `url`/`headers` — clients would read a broken hybrid.
		for k := range existing {
			if ownedEntryKeys[k] {
				if _, keep := entry[k]; !keep {
					delete(existing, k)
				}
			}
		}
		for k, v := range entry {
			if k == "headers" {
				existing[k] = mergeHeaders(existing[k], v)
				continue
			}
			existing[k] = v
		}
		return &mergePlan{action: planUpdate, path: path, before: raw, existed: existed, doc: doc}, nil
	}
}

// ownedEntryKeys are the fields a toolhost server entry may set — update
// may drop them when the new shape doesn't carry them, but must never
// touch keys the client or user added.
var ownedEntryKeys = map[string]bool{
	"command": true, "args": true, "type": true,
	"url": true, "serverUrl": true, "httpUrl": true, "headers": true,
}

// sameJSON compares via canonical JSON — the freshly-built entry uses
// typed maps while a round-tripped one is map[string]any; DeepEqual
// would see them as different.
func sameJSON(a, b any) bool {
	ra, ea := json.Marshal(a)
	rb, eb := json.Marshal(b)
	return ea == nil && eb == nil && bytes.Equal(ra, rb)
}

// looksLikeOurs reports whether an existing "toolhost" server entry was
// written by toolhost — either the HTTP shape (an endpoint key plus a
// bearer Authorization header) or the stdio spawn (a command whose args
// run "serve --stdio"). Anything else named toolhost belongs to somebody
// else.
func looksLikeOurs(entry map[string]any) bool {
	// HTTP shape: a bearer plus OUR /mcp endpoint path — a bare bearer
	// alone can't prove ownership; any authenticated MCP server matches.
	if h, ok := entry["headers"].(map[string]any); ok {
		if auth, ok := h["Authorization"].(string); ok && strings.HasPrefix(auth, "Bearer ") {
			for _, k := range []string{"url", "serverUrl", "httpUrl"} {
				if u, ok := entry[k].(string); ok && strings.HasSuffix(u, "/mcp") {
					return true
				}
			}
		}
	}
	// stdio shape: an argv that runs a toolhost-named binary with
	// "serve --stdio" — the spawn alone can't prove ownership either.
	// command is a string for most clients, an argv array for opencode.
	argv := entryArgv(entry)
	if len(argv) == 0 {
		return false
	}
	serve := false
	for i, a := range argv {
		if a == "serve" && i+1 < len(argv) && argv[i+1] == "--stdio" {
			serve = true
		}
	}
	return serve && strings.Contains(filepath.Base(argv[0]), "toolhost")
}

// entryArgv normalizes a client entry's spawn to one argv: command is a
// string (most clients) or an argv array (opencode), args appended when
// present.
func entryArgv(entry map[string]any) []string {
	var argv []string
	switch cmd := entry["command"].(type) {
	case string:
		argv = append(argv, cmd)
	case []any:
		for _, c := range cmd {
			if s, ok := c.(string); ok {
				argv = append(argv, s)
			}
		}
	}
	if args, ok := entry["args"].([]any); ok {
		for _, a := range args {
			if s, ok := a.(string); ok {
				argv = append(argv, s)
			}
		}
	}
	return argv
}

// commitMCPServerEntry writes the planned merge atomically: tmp + rename,
// but only after re-reading the file — if it changed since the plan was
// built (or appeared where none existed), the write is refused rather
// than silently lost-updated. 0600 — the entry can carry a bearer token.
// p.path is the symlink-resolved target from the plan.
func commitMCPServerEntry(p *mergePlan) error {
	cur, err := os.ReadFile(p.path)
	switch {
	case err == nil:
		if !p.existed {
			return fmt.Errorf("%s appeared while attaching — not overwriting; re-run attach", p.path)
		}
		if !bytes.Equal(cur, p.before) {
			return fmt.Errorf("%s changed while attaching — not overwriting; re-run attach", p.path)
		}
	case errors.Is(err, os.ErrNotExist):
		if p.existed {
			return fmt.Errorf("%s was deleted while attaching — not recreating; re-run attach", p.path)
		}
	default:
		return err
	}
	out, err := json.MarshalIndent(p.doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return err
	}
	tmp := p.path + ".tmp"
	// A leftover tmp keeps its old mode — unlink so WriteFile's 0600
	// actually applies; the file carries a bearer token.
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.path)
}

// mergeHeaders preserves client-added header keys while replacing the
// gateway's own: headers is gateway-owned at the entry level (a transport
// switch drops the whole key) but its contents are extras we must not
// clobber — a user's "X-Team" header survives an update while the stale
// Authorization bearer is replaced.
func mergeHeaders(old, new any) map[string]any {
	out := map[string]any{}
	for k, v := range headerPairs(old) {
		out[k] = v
	}
	for k, v := range headerPairs(new) {
		out[k] = v
	}
	return out
}

// headerPairs normalizes a headers value — map[string]string in a fresh
// entry, map[string]any after JSON round-trip.
func headerPairs(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out
	}
	return nil
}

// redactToken masks the gateway bearer in a copy of argv for display.
// Empty token (stdio attach) is a no-op — ReplaceAll on "" would corrupt
// every argument.
func redactToken(argv []string, token string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if token != "" {
			out[i] = strings.ReplaceAll(a, token, "***")
		} else {
			out[i] = a
		}
	}
	return out
}

// redactTokenBytes is redactToken for marshaled JSON — masks the bearer
// wherever it appears in a dry-run entry dump.
func redactTokenBytes(b []byte, token string) []byte {
	if token == "" {
		return b
	}
	return bytes.ReplaceAll(b, []byte(token), []byte("***"))
}

func devinArgs(url, token, bin, cfg string, stdio bool) []string {
	if stdio {
		return []string{"devin", "mcp", "add", "-s", "user", "toolhost", "--", bin, "serve", "--stdio", "-c", cfg}
	}
	return []string{"devin", "mcp", "add", "-s", "user", "toolhost", url, "-H", "Authorization: Bearer " + token}
}

// codexArgs registers through `codex mcp add`. The HTTP form never
// carries the token literal — codex resolves a named env var instead,
// which is also why the config's env: convention (TOOLHOST_TOKEN) is the
// name passed.
func codexArgs(url, token, bin, cfg string, stdio bool) []string {
	if stdio {
		return []string{"codex", "mcp", "add", "toolhost", "--", bin, "serve", "--stdio", "-c", cfg}
	}
	return []string{"codex", "mcp", "add", "toolhost", "--url", url, "--bearer-token-env-var", "TOOLHOST_TOKEN"}
}

// provisionSkill drops the control-plane SKILL.md into a client's
// user-level skill directory — the agent learns toolhost__search/enable/
// request without the human pasting instructions. The file is wholly
// ours: it carries a marker, refreshes freely on re-attach, and a
// markerless existing file is never touched.
func provisionSkill(t attachTarget, w io.Writer) {
	if t.skillDir == nil {
		return
	}
	dir, err := t.skillDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "SKILL.md")
	if raw, err := os.ReadFile(path); err == nil {
		if !bytes.Contains(raw, []byte(skillMarker)) {
			fmt.Fprintf(w, "note: %s exists and isn't ours — left untouched\n", path)
			return
		}
		if bytes.Equal(raw, []byte(skillDoc)) {
			return // already current — silent on re-attach
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(w, "note: cannot write %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, []byte(skillDoc), 0o644); err != nil {
		fmt.Fprintf(w, "note: cannot write %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(w, "wrote %s — %s learns the toolhost__* control plane\n", path, t.display)
}

// skillMarker identifies a SKILL.md written by attach — the ownership
// check that lets refreshes overwrite ours and refuse anything else.
const skillMarker = "<!-- installed by toolhost attach -->"

// skillDoc is the provisioned control-plane skill — the agent-facing
// manual for the toolhost__* tools. Kept generic: no repo paths, no
// machine specifics, so the same file serves every client and host.
const skillDoc = skillMarker + `
---
name: toolhost-gateway
description: How to use a toolhost MCP gateway — one governed /mcp endpoint fronting many MCP servers behind a three-tier tool gate (discovered/approved/enabled). Use when toolhost__* tools appear in the MCP surface, when a needed tool isn't available and a toolhost gateway may front it, or when asked to enable/request tools on a governed gateway.
---

# toolhost — the governed MCP gateway

toolhost fronts N upstream MCP servers behind one bearer-gated /mcp
endpoint. Agent-facing tools are namespaced ` + "`backend__tool`" + `. The gate:

` + "```" + `
discovered → approved (HUMAN policy) → enabled (live surface)
` + "```" + `

A tool not on the live surface is invisible AND uncallable — by design.

## If toolhost__* tools are in your surface

You are attached to a gateway. Self-serve — do not ask the human to edit
MCP config:

| tool | use |
|---|---|
| ` + "`toolhost__search`" + ` ` + "`{query}`" + ` | catalog search → qualified names + approved/enabled/requested state |
| ` + "`toolhost__list`" + ` ` + "`{scope}`" + ` | ` + "`enabled`" + ` (default) / ` + "`approved`" + ` / ` + "`all`" + ` |
| ` + "`toolhost__enable`" + ` ` + "`{names[]}`" + ` | move approved tools onto the live surface NOW |
| ` + "`toolhost__disable`" + ` ` + "`{names[]}`" + ` | pull tools off NOW (they stay approved) |
| ` + "`toolhost__call`" + ` ` + "`{name, arguments}`" + ` | call any enabled tool by name — works even if the client ignores tools/list_changed |
| ` + "`toolhost__request`" + ` ` + "`{names[], reason}`" + ` | file an approval ask — queued for the human, never self-approving |
| ` + "`toolhost__audit`" + ` ` + "`{limit?, kind?}`" + ` | tail the audit log — what was called, failed, refused |
| ` + "`toolhost__status`" + ` | read-only gateway health: mode, backends, counts, pending asks |

**The loop**: need a tool → ` + "`search`" + ` → if approved-but-disabled,
` + "`enable`" + ` it → call it → ` + "`disable`" + ` when done to keep the
surface lean.

**The boundary**: agents move tools between approved and enabled. Only
humans approve. If ` + "`search`" + ` shows ` + "`approved: false`" + `,
file ` + "`toolhost__request`" + ` with a reason — it lands where the
human sees it (` + "`toolhost status`" + `) — then the human runs
` + "`toolhost approve <name>`" + ` (or ` + "`toolhost approve --requested`" + `
to drain the queue). A request approves nothing by itself; never try to
route around it. Every enable/disable/call/request is audited.
`

func homeJoin(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}
