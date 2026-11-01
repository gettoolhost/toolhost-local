package app

import (
	"strings"
	"testing"
)

// mcp add grammar: flags anywhere, positional tail literal, `--` escapes.
func TestParseAdd(t *testing.T) {
	t.Run("flags anywhere + literal tail", func(t *testing.T) {
		a, err := parseAdd([]string{"-e", "K=v", "fs", "npx", "-y", "server", "/tmp", "--dry-run"})
		if err != nil {
			t.Fatal(err)
		}
		if a.name != "fs" || len(a.env) != 1 || !a.dryRun {
			t.Fatalf("got %+v", a)
		}
		if got := strings.Join(a.tail, " "); got != "npx -y server /tmp" {
			t.Fatalf("tail must be literal: %q", got)
		}
	})

	t.Run("lone -- is accepted and stripped", func(t *testing.T) {
		a, err := parseAdd([]string{"fs", "--", "npx", "-y", "server"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(a.tail, " "); got != "npx -y server" {
			t.Fatalf("tail: %q", got)
		}
	})

	t.Run("-- escapes flag collisions inside the server command", func(t *testing.T) {
		a, err := parseAdd([]string{"x", "--", "npx", "-e", "their-flag"})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(a.tail, " "); got != "npx -e their-flag" {
			t.Fatalf("their -e must survive literal: %q", got)
		}
	})

	t.Run("equals form", func(t *testing.T) {
		a, err := parseAdd([]string{"x", "--token=env:VAR", "https://u/mcp"})
		if err != nil || a.token != "env:VAR" {
			t.Fatalf("got %+v err=%v", a, err)
		}
	})

	t.Run("unknown flag refuses", func(t *testing.T) {
		if _, err := parseAdd([]string{"x", "--nope"}); err == nil {
			t.Fatal("unknown flag must refuse")
		}
	})
}

// tailBackend: url → http, bare words → stdio. env/headers/auth land in
// the right fields; env: refs stay literal.
func TestTailBackend(t *testing.T) {
	t.Run("url infers http", func(t *testing.T) {
		a := &addArgs{tail: []string{"https://x.com/mcp"}}
		b, err := a.tailBackend()
		if err != nil || b.Transport != "http" || b.URL != "https://x.com/mcp" {
			t.Fatalf("got %+v err=%v", b, err)
		}
	})

	t.Run("bare words infer stdio", func(t *testing.T) {
		a := &addArgs{tail: []string{"npx", "-y", "srv"}, env: []string{"A=1", "B=env:V"}}
		b, err := a.tailBackend()
		if err != nil || b.Transport != "stdio" || b.Command != "npx" ||
			len(b.Args) != 2 || b.Env["B"] != "env:V" {
			t.Fatalf("got %+v err=%v", b, err)
		}
	})

	t.Run("token becomes bearer auth", func(t *testing.T) {
		a := &addArgs{tail: []string{"https://x/mcp"}, token: "env:TOK"}
		b, err := a.tailBackend()
		if err != nil || b.Auth == nil || b.Auth.Type != "bearer" || b.Auth.Token != "env:TOK" {
			t.Fatalf("got %+v err=%v", b, err)
		}
	})

	t.Run("cc auth block", func(t *testing.T) {
		a := &addArgs{tail: []string{"https://x/mcp"}, auth: "cc",
			clientID: "me", clientSecret: "env:S", scopes: "a,b"}
		b, err := a.tailBackend()
		if err != nil || b.Auth.Type != "client_credentials" || len(b.Auth.Scopes) != 2 {
			t.Fatalf("got %+v err=%v", b, err)
		}
	})

	t.Run("junk after url refuses", func(t *testing.T) {
		a := &addArgs{tail: []string{"https://x/mcp", "stray"}}
		if _, err := a.tailBackend(); err == nil {
			t.Fatal("stray arg after url must refuse")
		}
	})
}

// entryToBackend normalizes each client dialect's server entry.
func TestEntryToBackend(t *testing.T) {
	cases := []struct {
		name, raw, wantT, wantCmd, wantURL string
	}{
		{"claude stdio", `{"command":"npx","args":["-y","srv"],"env":{"K":"v"}}`, "stdio", "npx", ""},
		{"claude http", `{"url":"https://x/mcp","headers":{"A":"b"}}`, "http", "", "https://x/mcp"},
		{"cursor serverUrl", `{"serverUrl":"https://y/mcp"}`, "http", "", "https://y/mcp"},
		{"opencode argv", `{"type":"local","command":["uvx","srv","--x"]}`, "stdio", "uvx", ""},
		{"opencode remote", `{"type":"remote","url":"https://z/mcp"}`, "http", "", "https://z/mcp"},
		{"vscode typed http", `{"type":"http","url":"https://w/mcp"}`, "http", "", "https://w/mcp"},
	}
	for _, c := range cases {
		b, err := entryToBackend([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if b.Transport != c.wantT || b.Command != c.wantCmd || b.URL != c.wantURL {
			t.Fatalf("%s: got %+v", c.name, b)
		}
	}
	if _, err := entryToBackend([]byte(`{"nothing":true}`)); err == nil {
		t.Fatal("entry with neither url nor command must refuse")
	}
}
