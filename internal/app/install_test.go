package app

// Unit generation is pure — the exec side effects are platform ops and
// stay untested here. What must be right: the resolved binary + config are
// baked in, the service restarts, and logs land beside the config.

import (
	"strings"
	"testing"
)

func TestLaunchdPlist(t *testing.T) {
	p := launchdPlist("/usr/local/bin/toolhost", "/home/u/toolhost.json", "/home/u/toolhost_serve.log")
	for _, want := range []string{
		"<key>Label</key><string>ai.toolhost.gateway</string>",
		"<string>/usr/local/bin/toolhost</string>",
		"<string>serve</string>",
		"<string>-c</string>",
		"<string>/home/u/toolhost.json</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		"<string>/home/u/toolhost_serve.log</string>",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("plist missing %q:\n%s", want, p)
		}
	}
}

func TestSystemdUnitBody(t *testing.T) {
	u := systemdUnitBody("/usr/local/bin/toolhost", "/home/u/toolhost.json")
	for _, want := range []string{
		"ExecStart=/usr/local/bin/toolhost serve -c /home/u/toolhost.json",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Fatalf("unit missing %q:\n%s", want, u)
		}
	}
}
