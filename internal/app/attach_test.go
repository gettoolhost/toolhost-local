package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServerEntry(t *testing.T) {
	http := serverEntry("http://127.0.0.1:8080/mcp", "th_x", "/bin/toolhost", "/c/toolhost.json", "url", false)
	if http["url"] != "http://127.0.0.1:8080/mcp" || http["type"] != "http" {
		t.Fatalf("http entry: %+v", http)
	}
	if http["headers"].(map[string]string)["Authorization"] != "Bearer th_x" {
		t.Fatalf("http entry missing bearer header: %+v", http)
	}
	if _, has := http["command"]; has {
		t.Fatal("http entry must not carry a command")
	}

	// Windsurf's legacy shape names the endpoint serverUrl, not url.
	ws := serverEntry("http://127.0.0.1:8080/mcp", "th_x", "/bin/toolhost", "/c/toolhost.json", "serverUrl", false)
	if ws["serverUrl"] != "http://127.0.0.1:8080/mcp" || ws["url"] != nil {
		t.Fatalf("windsurf entry: %+v", ws)
	}

	sp := serverEntry("", "", "/bin/toolhost", "/c/toolhost.json", "url", true)
	args := sp["args"].([]string)
	if sp["command"] != "/bin/toolhost" || args[0] != "serve" || args[1] != "--stdio" {
		t.Fatalf("stdio entry: %+v", sp)
	}
	// stdio entries never carry a token — the pipe's owner is the authority.
	raw, _ := json.Marshal(sp)
	if strings.Contains(string(raw), "Bearer") {
		t.Fatal("stdio entry must not carry credentials")
	}
}

func writeJSON(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestPlanMergeEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")

	// Existing config keeps other servers and unrelated keys.
	writeJSON(t, path, `{"mcpServers": {"other": {"url": "http://x"}}, "theme": "dark"}`)
	entry := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	p, err := planMCPServerEntry(path, "mcpServers", entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.action != planCreate {
		t.Fatalf("want create, got %s", p.action)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, path)
	servers := doc["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["local-gateway"] == nil {
		t.Fatalf("merge lost entries: %v", servers)
	}
	if doc["theme"] != "dark" {
		t.Fatal("merge dropped an unrelated key")
	}

	// Identical re-attach is a no-op — no write, no churn.
	p2, err := planMCPServerEntry(path, "mcpServers", entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if p2.action != planSkip {
		t.Fatalf("re-attach should be a no-op, got %s", p2.action)
	}

	// A stale entry of ours updates in place and keeps client-added keys.
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"url": "http://old/mcp", "headers": {"Authorization": "Bearer old"}, "disabled": true}}}`)
	fresh := serverEntry("http://127.0.0.1:9999/mcp", "th_new", "", "", "url", false)
	p3, err := planMCPServerEntry(path, "mcpServers", fresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if p3.action != planUpdate {
		t.Fatalf("want update, got %s", p3.action)
	}
	if err := commitMCPServerEntry(p3); err != nil {
		t.Fatal(err)
	}
	got := readDoc(t, path)["mcpServers"].(map[string]any)["local-gateway"].(map[string]any)
	if got["url"] != "http://127.0.0.1:9999/mcp" || got["disabled"] != true {
		t.Fatalf("update must refresh ours and keep extras: %v", got)
	}
}

func TestPlanForeignEntryRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	// A "local-gateway" entry that isn't ours — no bearer + endpoint, no
	// serve --stdio spawn. Overwriting would destroy someone's config.
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"command": "/opt/other/gw"}}}`)
	entry := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	if _, err := planMCPServerEntry(path, "mcpServers", entry, false); err == nil {
		t.Fatal("a foreign local-gateway entry must be refused, not clobbered")
	}

	// A foreign "toolhost" entry is a different name — we write
	// local-gateway beside it and leave it alone.
	writeJSON(t, path, `{"mcpServers": {"toolhost": {"command": "/opt/other/toolhostd"}}}`)
	p, err := planMCPServerEntry(path, "mcpServers", entry, false)
	if err != nil {
		t.Fatalf("a foreign toolhost entry must not block us: %v", err)
	}
	if p.action != planCreate {
		t.Fatalf("want create beside the foreign entry, got %s", p.action)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	servers := readDoc(t, path)["mcpServers"].(map[string]any)
	if servers["local-gateway"] == nil {
		t.Fatalf("local-gateway not created: %v", servers)
	}
	foreign, ok := servers["toolhost"].(map[string]any)
	if !ok || foreign["command"] != "/opt/other/toolhostd" {
		t.Fatalf("foreign toolhost entry was clobbered: %v", servers)
	}
}

func TestPlanConditionalAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created.json")
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), true)
	if err != nil {
		t.Fatal(err)
	}
	if p.action != planAbsent {
		t.Fatalf("conditional target with no file must skip, got %s", p.action)
	}
	// Unconditional targets still create the file.
	p2, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if p2.action != planCreate {
		t.Fatalf("unconditional target should create, got %s", p2.action)
	}
}

func TestCommitCASConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeJSON(t, path, `{"mcpServers": {}}`)
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	// Someone else writes between plan and commit — refuse, don't lose it.
	writeJSON(t, path, `{"mcpServers": {"theirs": {"url": "http://y"}}}`)
	if err := commitMCPServerEntry(p); err == nil {
		t.Fatal("commit must refuse when the file changed under us")
	}
	doc := readDoc(t, path)
	if doc["mcpServers"].(map[string]any)["theirs"] == nil {
		t.Fatal("lost-update: their write was clobbered")
	}
}

// Switching transports must not leave a hybrid entry — a stdio attach
// followed by an http attach would otherwise keep `command`/`args` beside
// `url`/`headers`, which clients read as stdio.
func TestPlanTransportSwitchDropsStaleKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"command": "/old/toolhost", "args": ["serve", "--stdio", "-c", "/c.json"], "disabled": true}}}`)
	http := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	p, err := planMCPServerEntry(path, "mcpServers", http, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.action != planUpdate {
		t.Fatalf("want update, got %s", p.action)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	got := readDoc(t, path)["mcpServers"].(map[string]any)["local-gateway"].(map[string]any)
	for _, stale := range []string{"command", "args"} {
		if got[stale] != nil {
			t.Fatalf("transport switch left stale %q: %v", stale, got)
		}
	}
	if got["url"] != "http://127.0.0.1:8188/mcp" || got["disabled"] != true {
		t.Fatalf("update must write ours and keep extras: %v", got)
	}

	// And the reverse — http → stdio drops url/headers.
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"type": "http", "url": "http://x/mcp", "headers": {"Authorization": "Bearer t"}}}}`)
	sp := serverEntry("", "", "/b/toolhost", "/c.json", "url", true)
	p2, err := planMCPServerEntry(path, "mcpServers", sp, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitMCPServerEntry(p2); err != nil {
		t.Fatal(err)
	}
	got = readDoc(t, path)["mcpServers"].(map[string]any)["local-gateway"].(map[string]any)
	for _, stale := range []string{"url", "headers"} {
		if got[stale] != nil {
			t.Fatalf("http→stdio left stale %q: %v", stale, got)
		}
	}
	if got["command"] != "/b/toolhost" {
		t.Fatalf("stdio entry missing command: %v", got)
	}
}

// A file appearing between plan and commit (where none existed) is a CAS
// conflict — empty content is still someone else's write.
func TestCommitRefusesAppearedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	writeJSON(t, path, "") // appeared, empty
	if err := commitMCPServerEntry(p); err == nil {
		t.Fatal("commit must refuse a file that appeared mid-attach")
	}
}

func TestPlanNewFilePerms(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "sub", "mcp.json")
	entry := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	p, err := planMCPServerEntry(fresh, "mcpServers", entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("client config with a token must be 0600, got %o", fi.Mode().Perm())
	}
}

func TestPlanCorruptFailsClosed(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "broken.json")
	writeJSON(t, bad, "{nope")
	if _, err := planMCPServerEntry(bad, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false); err == nil {
		t.Fatal("corrupt client config should fail, not be overwritten")
	}
}

// A config file containing the literal "null" is valid JSON that
// unmarshals into a nil map — it must be treated as empty, not panic.
func TestPlanNullConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "null.json")
	writeJSON(t, path, "null")
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if p.action != planCreate {
		t.Fatalf("null config should plan a create, got %s", p.action)
	}
}

// A "local-gateway" key that isn't an object is foreign data — refuse,
// never silently overwrite it. A non-object "toolhost" is someone else's
// server too, but it's a different name — we coexist, we don't rewrite it.
func TestPlanNonObjectEntryRefused(t *testing.T) {
	for _, body := range []string{
		`{"mcpServers": {"local-gateway": "http://someone-else"}}`,
		`{"mcpServers": {"local-gateway": false}}`,
		`{"mcpServers": {"local-gateway": ["x"]}}`,
	} {
		path := filepath.Join(t.TempDir(), "mcp.json")
		writeJSON(t, path, body)
		if _, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false); err == nil {
			t.Fatalf("non-object local-gateway entry must be refused: %s", body)
		}
	}
	// Non-object legacy key — leave it, write ours beside it.
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path, `{"mcpServers": {"toolhost": "http://someone-else"}}`)
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatalf("foreign legacy entry must not block us: %v", err)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	servers := readDoc(t, path)["mcpServers"].(map[string]any)
	if servers["local-gateway"] == nil || servers["toolhost"] != "http://someone-else" {
		t.Fatalf("must create ours and preserve theirs: %v", servers)
	}
}

// A non-object mcpServers key is malformed client config — refuse rather
// than replace the value.
func TestPlanNonObjectTopKeyRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path, `{"mcpServers": "everything"}`)
	if _, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false); err == nil {
		t.Fatal("non-object mcpServers must be refused")
	}
}

// A symlinked config must be written through the link — renaming over
// the link would sever it.
func TestPlanWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "linked.json")
	writeJSON(t, real, `{"mcpServers": {}}`)
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	p, err := planMCPServerEntry(link, "mcpServers", serverEntry("http://x/mcp", "t", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("attach severed the symlink — must write through it")
	}
	doc := readDoc(t, real)
	if doc["mcpServers"].(map[string]any)["local-gateway"] == nil {
		t.Fatal("entry missing after symlink write-through")
	}
}

// VS Code's mcp.json namespaces servers under "servers", not "mcpServers".
func TestPlanTopKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path, `{"servers": {"other": {"url": "http://x"}}}`)
	entry := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	p, err := planMCPServerEntry(path, "servers", entry, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, path)
	if doc["servers"].(map[string]any)["local-gateway"] == nil || doc["mcpServers"] != nil {
		t.Fatalf("entry must land under \"servers\": %v", doc)
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"https://mcp.example.com/mcp":  "mcp.example.com:443",
		"http://127.0.0.1:8188/mcp":    "127.0.0.1:8188",
		"http://plain.example.com/sse": "plain.example.com:80",
	} {
		if got := hostPort(in); got != want {
			t.Fatalf("hostPort(%q): want %q, got %q", in, want, got)
		}
	}
}

// The echoed argv (running:/would run:) must never carry the bearer —
// exec gets the real token, scrollback gets ***.
func TestRedactToken(t *testing.T) {
	argv := devinArgs("http://gw:8080/mcp", "sekrit", "toolhost", "/c.json", false)
	got := redactToken(argv, "sekrit")
	for _, a := range got {
		if strings.Contains(a, "sekrit") {
			t.Fatalf("token leaked into display argv: %q", a)
		}
	}
	if !strings.Contains(strings.Join(got, " "), "Bearer ***") {
		t.Fatalf("expected redacted bearer, got %v", got)
	}
	// argv itself is untouched — exec still receives the real token.
	if !strings.Contains(argv[len(argv)-1], "sekrit") {
		t.Fatal("redactToken mutated the exec argv")
	}
	// Empty token (stdio attach) is a no-op, not a corruption.
	argv = devinArgs("", "", "toolhost", "/c.json", true)
	if got := redactToken(argv, ""); !reflect.DeepEqual(got, argv) {
		t.Fatalf("empty token must pass argv through: %v", got)
	}
}

// A stale entry's headers map holds gateway-owned Authorization plus
// client-added keys — update must merge: refresh the bearer, keep extras.
func TestUpdatePreservesClientHeaderKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"url": "http://old/mcp", "headers": {"Authorization": "Bearer old", "X-Team": "keep-me"}}}}`)
	fresh := serverEntry("http://127.0.0.1:9999/mcp", "th_new", "", "", "url", false)
	p, err := planMCPServerEntry(path, "mcpServers", fresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.action != planUpdate {
		t.Fatalf("want update, got %s", p.action)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	h := readDoc(t, path)["mcpServers"].(map[string]any)["local-gateway"].(map[string]any)["headers"].(map[string]any)
	if h["X-Team"] != "keep-me" {
		t.Fatalf("update dropped a client header key: %v", h)
	}
	if h["Authorization"] != "Bearer th_new" {
		t.Fatalf("bearer not refreshed: %v", h)
	}
}

// attach --dry-run must never print the gateway bearer for file clients —
// same rule as the CLI branch.
func TestDryRunRedactsBearer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "toolhost.json")
	if err := os.WriteFile(cfg, []byte(`{"token":"th_sekrit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Attach(cfg, "cursor", false, false, false, true, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "th_sekrit") {
		t.Fatalf("dry-run leaked the bearer:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Bearer ***") {
		t.Fatalf("expected a redacted bearer in output:\n%s", buf.String())
	}
}

// A foreign entry that merely carries a bearer + url is not ours — the
// heuristic needs our /mcp path or a toolhost-named spawn, else we'd
// clobber an unrelated authenticated server.
func TestLooksLikeOursRejectsForeignShapes(t *testing.T) {
	// Generic authenticated HTTP server — bearer alone proves nothing.
	path := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path, `{"mcpServers": {"local-gateway": {"url": "https://api.other.io/v1", "headers": {"Authorization": "Bearer their-key"}}}}`)
	if _, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "th_k", "", "", "url", false), false); err == nil {
		t.Fatal("a foreign bearer+url entry must be refused, not refreshed")
	}
	// Another binary that happens to take serve --stdio args.
	path2 := filepath.Join(t.TempDir(), "mcp.json")
	writeJSON(t, path2, `{"mcpServers": {"local-gateway": {"command": "/opt/otheR-mcpd", "args": ["serve", "--stdio"]}}}`)
	if _, err := planMCPServerEntry(path2, "mcpServers", serverEntry("", "", "/bin/toolhost", "/c", "url", true), false); err == nil {
		t.Fatal("a foreign serve --stdio spawn must be refused")
	}
}

// The new entry builders each emit their client's own shape — and never
// carry credentials in a spawn form.
func TestClientEntryShapes(t *testing.T) {
	// opencode: remote = type+url+headers, local = argv-array command.
	oc := opencodeEntry("http://127.0.0.1:8080/mcp", "th_k", "/bin/toolhost", "/c/toolhost.json", "url", false)
	if oc["type"] != "remote" || oc["url"] != "http://127.0.0.1:8080/mcp" {
		t.Fatalf("opencode remote entry: %+v", oc)
	}
	if h, ok := oc["headers"].(map[string]string); !ok || h["Authorization"] != "Bearer th_k" {
		t.Fatalf("opencode remote missing bearer: %+v", oc)
	}
	ocl := opencodeEntry("", "", "/bin/toolhost", "/c/toolhost.json", "url", true)
	if ocl["type"] != "local" {
		t.Fatalf("opencode local entry: %+v", ocl)
	}
	if cmd, ok := ocl["command"].([]string); !ok || cmd[0] != "/bin/toolhost" || cmd[1] != "serve" {
		t.Fatalf("opencode local command must be an argv array: %+v", ocl)
	}
	if _, has := ocl["enabled"]; has {
		t.Fatal("opencode entry must not write enabled — absent already means enabled")
	}

	// zed: remote = url+headers (no "type"); local = bare command string.
	zr := zedEntry("http://127.0.0.1:8080/mcp", "th_k", "/bin/toolhost", "/c", "url", false)
	if zr["url"] != "http://127.0.0.1:8080/mcp" || zr["type"] != nil {
		t.Fatalf("zed remote entry: %+v", zr)
	}
	zl := zedEntry("", "", "/bin/toolhost", "/c", "url", true)
	if zl["command"] != "/bin/toolhost" {
		t.Fatalf("zed local command must be a bare string: %+v", zl)
	}

	// claude-desktop: stdio shape even when stdio=false — it can't do remote.
	cd := claudeDesktopEntry("http://ignored/mcp", "th_k", "/bin/toolhost", "/c", "url", false)
	if cd["command"] != "/bin/toolhost" {
		t.Fatalf("claude-desktop must emit the spawn form: %+v", cd)
	}
	if raw, _ := json.Marshal(cd); strings.Contains(string(raw), "Bearer") {
		t.Fatal("claude-desktop entry must not carry credentials")
	}
}

// opencode's stdio entry puts the whole spawn in "command" as an argv
// array — looksLikeOurs must still recognize it as ours on re-attach.
func TestLooksLikeOursArgvCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	// A stale spawn (different config path) plus a client-added key — not
	// identical, so ownership decides update vs refusal.
	writeJSON(t, path, `{"mcp": {"local-gateway": {"type": "local", "command": ["/opt/toolhost", "serve", "--stdio", "-c", "/old/toolhost.json"], "enabled": false}}}`)
	p, err := planMCPServerEntry(path, "mcp", opencodeEntry("", "", "/opt/toolhost", "/c/toolhost.json", "url", true), false)
	if err != nil {
		t.Fatalf("a stale argv-command entry must update, not refuse: %v", err)
	}
	if p.action != planUpdate {
		t.Fatalf("want update, got %s", p.action)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	got := readDoc(t, path)["mcp"].(map[string]any)["local-gateway"].(map[string]any)
	if got["enabled"] != false {
		t.Fatalf("update must keep the client's enabled flag: %v", got)
	}

	// But an argv spawn of a NON-toolhost binary under our name stays
	// foreign.
	writeJSON(t, path, `{"mcp": {"local-gateway": {"type": "local", "command": ["/opt/otheR-mcpd", "serve", "--stdio"]}}}`)
	if _, err := planMCPServerEntry(path, "mcp", opencodeEntry("", "", "/opt/toolhost", "/c", "url", true), false); err == nil {
		t.Fatal("a foreign argv spawn must be refused")
	}
}

// provisionSkill writes our SKILL.md, refreshes it when stale, and never
// clobbers a file that isn't ours.
func TestProvisionSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tgt := attachTargets["claude"]

	var buf bytes.Buffer
	provisionSkill(tgt, &buf)
	path := filepath.Join(home, ".claude", "skills", "toolhost-gateway", "SKILL.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("skill not written: %v", err)
	}
	if !strings.Contains(string(raw), skillMarker) || !strings.Contains(string(raw), "toolhost__enable") {
		t.Fatal("skill doc missing marker or control-plane content")
	}

	// A foreign file at the same path is left alone, with a note.
	if err := os.WriteFile(path, []byte("user's own notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	provisionSkill(tgt, &buf)
	if raw, _ := os.ReadFile(path); string(raw) != "user's own notes" {
		t.Fatal("provisionSkill clobbered a file that isn't ours")
	}
	if !strings.Contains(buf.String(), "isn't ours") {
		t.Fatalf("expected an untouched note, got: %s", buf.String())
	}
}

// Untouched fields must round-trip exactly — a float64 decode loses
// precision on ints > 2^53 (nanos timestamps, big IDs in ~/.claude.json).
func TestCommitPreservesLargeIntegers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	writeJSON(t, path, `{"installId": 9007199254740993, "sessionStartNanos": 1757000000000000123, "mcpServers": {}}`)
	p, err := planMCPServerEntry(path, "mcpServers", serverEntry("http://x/mcp", "th_k", "", "", "url", false), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitMCPServerEntry(p); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, lit := range []string{"9007199254740993", "1757000000000000123"} {
		if !strings.Contains(string(raw), lit) {
			t.Fatalf("big integer corrupted on merge — want %s in:\n%s", lit, raw)
		}
	}
}
