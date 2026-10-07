package app

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestMergeMCPServerEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")

	// Existing config keeps other servers and unrelated keys.
	existing := `{"mcpServers": {"other": {"url": "http://x"}}, "theme": "dark"}`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := serverEntry("http://127.0.0.1:8188/mcp", "th_k", "", "", "url", false)
	if err := mergeMCPServerEntry(path, entry); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["toolhost"] == nil {
		t.Fatalf("merge lost entries: %s", raw)
	}
	if doc["theme"] != "dark" {
		t.Fatalf("merge dropped an unrelated key: %s", raw)
	}

	// Fresh file: created with 0600 — the entry carries a bearer token.
	fresh := filepath.Join(dir, "sub", "mcp.json")
	if err := mergeMCPServerEntry(fresh, entry); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("client config with a token must be 0600, got %o", fi.Mode().Perm())
	}

	// Corrupt existing config fails closed — never clobber.
	bad := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeMCPServerEntry(bad, entry); err == nil {
		t.Fatal("corrupt client config should fail, not be overwritten")
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
