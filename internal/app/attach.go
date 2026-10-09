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
		if dryRun {
			fmt.Fprintf(w, "would run: %s\n", strings.Join(t.cli(url, token, bin, abs, stdio), " "))
			return nil
		}
		argv := t.cli(url, token, bin, abs, stdio)
		fmt.Fprintf(w, "running: %s\n", strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = w, w
		return cmd.Run()
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
			fmt.Fprintf(w, "  \"toolhost\": %s\n", raw)
		}
		return nil
	}
	switch res.action {
	case planSkip:
		fmt.Fprintf(w, "toolhost already attached in %s — nothing to do\n", path)
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
	return nil
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
	},
	"devin": {
		display: "Devin",
		cli:     devinArgs,
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
		if err := json.Unmarshal(raw, &doc); err != nil {
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
	if h, ok := entry["headers"].(map[string]any); ok {
		if auth, ok := h["Authorization"].(string); ok && strings.HasPrefix(auth, "Bearer ") {
			for _, k := range []string{"url", "serverUrl", "httpUrl"} {
				if _, ok := entry[k].(string); ok {
					return true
				}
			}
		}
	}
	args, _ := entry["args"].([]any)
	serve := false
	for i, a := range args {
		if a == "serve" && i+1 < len(args) && args[i+1] == "--stdio" {
			serve = true
		}
	}
	_, hasCmd := entry["command"].(string)
	return hasCmd && serve
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

func devinArgs(url, token, bin, cfg string, stdio bool) []string {
	if stdio {
		return []string{"devin", "mcp", "add", "-s", "user", "toolhost", "--", bin, "serve", "--stdio", "-c", cfg}
	}
	return []string{"devin", "mcp", "add", "-s", "user", "toolhost", url, "-H", "Authorization: Bearer " + token}
}

func homeJoin(elem ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{home}, elem...)...), nil
}
