package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/oauth"
)

// Doctor checks an install end to end without needing the gateway to be
// running — the setup counterpart of status (which asks the live gateway).
// Each check prints one line; any failure returns an error so scripts can
// gate on `toolhost doctor`.
func Doctor(ctx context.Context, cfgPath string, w io.Writer) error {
	var failed int
	ok := func(cond bool, good, bad string) {
		if cond {
			fmt.Fprintf(w, "✓ %s\n", good)
		} else {
			fmt.Fprintf(w, "✗ %s\n", bad)
			failed++
		}
	}

	f, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(w, "✗ config: %v\n", err)
		fmt.Fprintf(w, "  fix: toolhost init -c %s\n", cfgPath)
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	fmt.Fprintf(w, "✓ config %s parses\n", cfgPath)

	token, err := f.ResolvedToken()
	ok(err == nil && token != "", "gateway token resolves",
		fmt.Sprintf("gateway token: %v — set \"token\" or its env: ref", err))

	// The port is either serving (already up), free, or bogus.
	if _, _, err := net.SplitHostPort(f.Listen); err != nil {
		fmt.Fprintf(w, "✗ listen %q is not host:port\n", f.Listen)
		failed++
	} else {
		conn, derr := net.DialTimeout("tcp", f.Listen, 2*time.Second)
		switch {
		case derr == nil:
			conn.Close()
			fmt.Fprintf(w, "✓ %s is listening — gateway already up\n", f.Listen)
		case isConnRefused(derr):
			fmt.Fprintf(w, "- %s free — run toolhost serve or toolhost install\n", f.Listen)
		default:
			fmt.Fprintf(w, "✗ listen %s: %v\n", f.Listen, derr)
			failed++
		}
	}

	// Audit log's directory must be writable — anchored to the config
	// dir, exactly as serve resolves it. Probes with a temp file rather
	// than mkdir: a diagnostic shouldn't mutate the filesystem.
	auditPath := f.AuditLog
	if !filepath.IsAbs(auditPath) {
		auditPath = filepath.Join(filepath.Dir(cfgPath), auditPath)
	}
	auditDir := filepath.Dir(auditPath)
	fi, err := os.Stat(auditDir)
	switch {
	case err == nil && !fi.IsDir():
		// Exists but isn't a directory — serve's MkdirAll will fail
		// outright; that's a broken config, not an absent dir.
		fmt.Fprintf(w, "✗ audit dir %s is not a directory\n", auditDir)
		failed++
	case err != nil:
		fmt.Fprintf(w, "- audit dir %s absent — serve will create it\n", auditDir)
	default:
		probe, err := os.CreateTemp(auditDir, ".doctor-*")
		if err != nil {
			fmt.Fprintf(w, "✗ audit dir %s: %v\n", auditDir, err)
			failed++
		} else {
			probe.Close()
			os.Remove(probe.Name())
			fmt.Fprintf(w, "✓ audit log dir %s writable\n", auditDir)
		}
	}

	// Backends: transport-level sanity, not a full MCP handshake — doctor
	// answers "would this connect", not "does it work".
	tokens, _ := oauth.OpenStore(oauth.StorePath(cfgPath))
	for _, name := range sortedKeys(f.Backends) {
		b := f.Backends[name]
		switch b.Transport {
		case "stdio":
			_, err := exec.LookPath(b.Command)
			ok(err == nil, fmt.Sprintf("backend %-10s stdio %q on PATH", name, b.Command),
				fmt.Sprintf("backend %s: command %q not found on PATH", name, b.Command))
		default:
			conn, derr := net.DialTimeout("tcp", hostPort(b.URL), 3*time.Second)
			if derr == nil {
				conn.Close()
				fmt.Fprintf(w, "✓ backend %-10s reachable at %s\n", name, b.URL)
			} else {
				fmt.Fprintf(w, "✗ backend %s unreachable at %s: %v\n", name, b.URL, derr)
				failed++
			}
		}
		if b.Auth != nil && b.Auth.Type == "oauth" {
			ok(tokens != nil && tokens.Get(name) != nil,
				fmt.Sprintf("backend %-10s oauth grant stored", name),
				fmt.Sprintf("backend %s: oauth grant missing — run toolhost auth %s", name, name))
		}
	}
	if len(f.Backends) == 0 {
		fmt.Fprintln(w, "- no backends configured — edit backends in "+cfgPath)
	}

	// Service registration — informational, not a failure either way.
	switch runtime.GOOS {
	case "darwin":
		err := exec.Command("launchctl", "print", "gui/"+uid()+"/"+launchdLabel).Run()
		if err == nil {
			fmt.Fprintf(w, "✓ service %s installed\n", launchdLabel)
		} else {
			fmt.Fprintf(w, "- service not installed (toolhost install to persist)\n")
		}
	case "linux":
		unit := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", systemdUnit)
		if _, err := os.Stat(unit); err == nil {
			fmt.Fprintf(w, "✓ service %s installed\n", systemdUnit)
		} else {
			fmt.Fprintf(w, "- service not installed (toolhost install to persist)\n")
		}
	}

	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	fmt.Fprintln(w, "all checks passed")
	return nil
}

func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}

// hostPort extracts the dialable host:port from an http(s) URL, defaulting
// the port by scheme.
func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return u.Host + ":443"
	}
	return u.Host + ":80"
}
