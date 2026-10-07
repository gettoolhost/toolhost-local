package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Attach registers the gateway with an MCP client — the last mile of
// install. Instead of hand-editing each client's JSON or memorizing its
// CLI syntax, one command writes (or prints) the entry. File-backed
// clients get an mcpServers merge; CLI-backed clients get their `mcp add`
// invocation executed. --stdio attaches the spawn form (serve --stdio)
// instead of the HTTP endpoint + bearer.
func Attach(cfgPath, client string, stdio, printOnly bool, w io.Writer) error {
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
		fmt.Fprintf(w, "running: %s\n", strings.Join(argv, " "))
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Stdout, cmd.Stderr = w, w
		return cmd.Run()
	}
	path, err := t.file()
	if err != nil {
		return err
	}
	if err := mergeMCPServerEntry(path, serverEntry(url, token, bin, abs, t.urlField, stdio)); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote toolhost server entry to %s\n", path)
	if !stdio {
		fmt.Fprintf(w, "gateway must be serving: toolhost serve (or toolhost install)\n")
	}
	return nil
}

// attachTarget is one MCP client's attach strategy: a CLI to run, or a
// JSON file whose mcpServers map gains a "toolhost" entry. urlField is the
// client's name for the endpoint key ("url" for most, "serverUrl" for
// windsurf's legacy shape).
type attachTarget struct {
	cli      func(url, token, bin, cfg string, stdio bool) []string
	file     func() (string, error)
	urlField string
}

var attachTargets = map[string]attachTarget{
	"claude": {cli: claudeArgs},
	"devin":  {cli: devinArgs},
	"cursor": {file: func() (string, error) {
		return homeJoin(".cursor", "mcp.json")
	}},
	"windsurf": {file: func() (string, error) {
		return homeJoin(".codeium", "windsurf", "mcp_config.json")
	}, urlField: "serverUrl"},
}

func attachTargetNames() string {
	names := make([]string, 0, len(attachTargets))
	for n := range attachTargets {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// serverEntry is the mcpServers fragment for this gateway — either the
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

// mergeMCPServerEntry writes "toolhost" into a client's JSON config,
// preserving every other server and key. 0600 — the entry carries a token.
func mergeMCPServerEntry(path string, entry map[string]any) error {
	doc := map[string]any{}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	servers, _ := doc["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
		doc["mcpServers"] = servers
	}
	servers["toolhost"] = entry
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func claudeArgs(url, token, bin, cfg string, stdio bool) []string {
	if stdio {
		return []string{"claude", "mcp", "add", "toolhost", "-s", "user", "--", bin, "serve", "--stdio", "-c", cfg}
	}
	return []string{"claude", "mcp", "add", "toolhost", "--transport", "http", "-s", "user", url, "-H", "Authorization: Bearer " + token}
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
