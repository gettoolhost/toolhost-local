package app

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Install registers (or removes) toolhost serve as a user service so the
// gateway survives logout and reboot — launchd on macOS, systemd --user on
// Linux. The unit runs the same binary and config this command was invoked
// with; on the gateway's own log discipline (stdout is captured to
// toolhost_serve.log beside the config).
func Install(cfgPath string, install bool, w io.Writer) error {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return err
	}
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}
	if strings.Contains(bin, "go-build") || strings.Contains(bin, "/tmp/go") {
		fmt.Fprintf(w, "warning: %s looks like a go run temp binary — build first (go build -o toolhost ./cmd/toolhost)\n", bin)
	}

	switch runtime.GOOS {
	case "darwin":
		return installLaunchd(bin, abs, install, w)
	case "linux":
		return installSystemd(bin, abs, install, w)
	default:
		return fmt.Errorf("install: unsupported platform %s — run manually: %s serve -c %s", runtime.GOOS, bin, abs)
	}
}

const launchdLabel = "ai.toolhost.gateway"

func installLaunchd(bin, cfg string, install bool, w io.Writer) error {
	dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
	plist := filepath.Join(dir, launchdLabel+".plist")
	logPath := strings.TrimSuffix(cfg, filepath.Ext(cfg)) + "_serve.log"

	if !install {
		_ = exec.Command("launchctl", "bootout", "gui/"+uid()+"/"+launchdLabel).Run()
		if err := os.Remove(plist); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintf(w, "removed %s (service stopped if it was running)\n", plist)
		return nil
	}

	if _, err := os.Stat(cfg); err != nil {
		return fmt.Errorf("config %s: %w", cfg, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plist, []byte(launchdPlist(bin, cfg, logPath)), 0o644); err != nil {
		return err
	}

	// bootout first so re-installs pick up the new unit; a missing service
	// errors bootout — harmless.
	_ = exec.Command("launchctl", "bootout", "gui/"+uid()+"/"+launchdLabel).Run()
	if out, err := exec.Command("launchctl", "bootstrap", "gui/"+uid(), plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "installed %s — serving %s (log: %s)\n", launchdLabel, cfg, logPath)
	fmt.Fprintln(w, "check:  launchctl print gui/$(id -u)/"+launchdLabel)
	fmt.Fprintln(w, "remove: toolhost uninstall")
	return nil
}

// launchdPlist is the launchd agent unit: run at load, keep alive, both
// output streams to the serve log beside the config.
func launchdPlist(bin, cfg, logPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>-c</string>
		<string>%s</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchdLabel, bin, cfg, logPath, logPath)
}

const systemdUnit = "toolhost.service"

func installSystemd(bin, cfg string, install bool, w io.Writer) error {
	dir := filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user")
	unit := filepath.Join(dir, systemdUnit)

	if !install {
		_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnit).Run()
		if err := os.Remove(unit); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		fmt.Fprintf(w, "removed %s (service stopped if it was running)\n", unit)
		return nil
	}

	if _, err := os.Stat(cfg); err != nil {
		return fmt.Errorf("config %s: %w", cfg, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unit, []byte(systemdUnitBody(bin, cfg)), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", systemdUnit).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %w: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(w, "installed %s — serving %s\n", systemdUnit, cfg)
	fmt.Fprintln(w, "check:  systemctl --user status toolhost")
	fmt.Fprintln(w, "remove: toolhost uninstall")
	return nil
}

// systemdUnitBody is the user-service unit — restart on failure, the
// resolved binary and config baked in.
func systemdUnitBody(bin, cfg string) string {
	return fmt.Sprintf(`[Unit]
Description=toolhost MCP gateway

[Service]
ExecStart=%s serve -c %s
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
`, bin, cfg)
}

func uid() string {
	out, err := exec.Command("id", "-u").Output()
	if err != nil {
		return "$(id -u)"
	}
	return strings.TrimSpace(string(out))
}
