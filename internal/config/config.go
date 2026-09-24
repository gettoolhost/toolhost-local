// Package config is the driven adapter for the JSON config file — the only
// state toolhost keeps. Approval is a list of qualified names in this file;
// `toolhost approve` edits it, `serve` reads it at boot.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/namespace"
)

// Backend describes one upstream MCP server.
type Backend struct {
	// Transport is "stdio" (local subprocess), "http" (remote streamable-HTTP
	// MCP server), or "sse" (legacy HTTP+SSE MCP server).
	Transport string `json:"transport"`

	// stdio transport:
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`

	// http/sse transports:
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Auth    *Auth             `json:"auth,omitempty"`

	// Passthrough marks this upstream as a federated toolhost gateway: its
	// tool names arrive already qualified ("cbm__search_graph") and are
	// trusted as-is instead of being re-namespaced. Its own toolhost__*
	// meta-tools are dropped — the outer gateway has its own control plane.
	Passthrough bool `json:"passthrough,omitempty"`

	// CallTimeout overrides the top-level call_timeout for this backend —
	// a Go duration ("30s", "2m"). A client-supplied tighter deadline still
	// wins; this bounds how long a hung backend may hold a call.
	CallTimeout string `json:"call_timeout,omitempty"`
}

// Auth configures how toolhost authenticates TO an upstream server —
// orthogonal to the bearer token that authenticates agents to toolhost.
type Auth struct {
	// Type: "none" (default), "bearer" (a static token sent as
	// Authorization: Bearer — the API-key shape), "client_credentials"
	// (machine OAuth; token endpoint discovered via the server's Protected
	// Resource Metadata), "oauth" (interactive auth-code + PKCE; run
	// `toolhost auth <backend>` to grant).
	Type string `json:"type"`

	// bearer:
	Token string `json:"token,omitempty"`

	// client_credentials + oauth:
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`

	// oauth only: loopback redirect the AS redirects the browser to.
	// Must match the registered/registered-via-DCR redirect URI.
	RedirectURL string `json:"redirect_url,omitempty"`
}

// File is the on-disk config document.
type File struct {
	Listen   string              `json:"listen"`
	Token    string              `json:"token"`
	AuditLog string              `json:"audit_log"`
	Backends map[string]*Backend `json:"backends"`
	// Mode selects the front-door serving mode — "stateless" (default) or
	// "stateful". Stateless is the primary design (SEP-2567 sessionless +
	// the 2026-07-28 spec): no Mcp-Session-Id, every request independent.
	// Legacy-protocol clients still work sessionlessly there — initialize,
	// tools/list, tools/call each stand alone; what they lose is pushed
	// tools/list_changed, which needs a held session. "stateful" is the
	// opt-in compat mode for exactly that: session-bearing Streamable HTTP
	// for clients that must hold Mcp-Session-Id.
	Mode string `json:"mode,omitempty"`
	// Approved is the qualified-name list ("backend__tool") that Resolve
	// intersects with discovery. discovered ≠ approved: everything not on
	// this list is invisible and uncallable.
	Approved []string `json:"approved"`
	// Enabled is the live-surface allowlist, third tier of the invariant:
	// nil (absent) means every approved tool is enabled; a non-nil list
	// means only enabled∩approved is visible. Pointer so "absent" and
	// "explicitly empty" stay distinct — [] must not collapse into all.
	Enabled *[]string `json:"enabled,omitempty"`

	// CallTimeout bounds every upstream tool call — a Go duration,
	// default "60s". A caller's tighter deadline still wins. Per-backend
	// call_timeout overrides it. A hung backend can never hold a call
	// forever.
	CallTimeout string `json:"call_timeout,omitempty"`

	// Requested is the agent's pending ask: qualified names an agent put
	// on the record via toolhost__request, with its reason. Humans review
	// it (`toolhost status`) and approve consumes entries. Agents can
	// write it; only humans turn requests into approvals.
	Requested []Request `json:"requested,omitempty"`
}

// Request is one agent-filed approval request.
type Request struct {
	Name   string    `json:"name"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultAuditLog = "toolhost_audit.jsonl"

	// ModeStateless is the default serving mode — sessionless Streamable
	// HTTP. ModeStateful opts into session-bearing mode for clients that
	// need pushed notifications/tools/list_changed.
	ModeStateless = "stateless"
	ModeStateful  = "stateful"

	// DefaultCallTimeout bounds upstream calls when the config doesn't say.
	DefaultCallTimeout = "60s"
)

func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	f.applyDefaults()
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &f, nil
}

func (f *File) applyDefaults() {
	if f.Listen == "" {
		f.Listen = DefaultListen
	}
	if f.Mode == "" {
		f.Mode = ModeStateless
	}
	if f.AuditLog == "" {
		f.AuditLog = DefaultAuditLog
	}
	if f.CallTimeout == "" {
		f.CallTimeout = DefaultCallTimeout
	}
	if f.Backends == nil {
		f.Backends = map[string]*Backend{}
	}
	if f.Approved == nil {
		f.Approved = []string{}
	}
}

// ResolveEnv expands an "env:NAME" reference; anything else passes through
// unchanged. Unset or empty fails closed — a config pointing at a missing
// variable is broken, not merely incomplete.
//
// Credential fields stay as references inside File — Save round-trips the
// reference, never the resolved secret, so toolhost.json can carry
// pointers and be committed safely. Resolution happens at the boundary:
// ResolvedToken for the gateway token, Backend.Resolved for upstream creds.
func ResolveEnv(s string) (string, error) {
	if !strings.HasPrefix(s, "env:") {
		return s, nil
	}
	name := strings.TrimPrefix(s, "env:")
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("env var %s is unset or empty", name)
}

// ResolvedToken resolves the gateway bearer token.
func (f *File) ResolvedToken() (string, error) {
	return ResolveEnv(f.Token)
}

// Resolved returns a copy of the backend with every "env:" reference in
// headers and auth fields resolved. The receiver is unchanged — the file's
// in-memory shape keeps references, never secrets.
func (b *Backend) Resolved() (*Backend, error) {
	c := *b
	if b.Headers != nil {
		c.Headers = make(map[string]string, len(b.Headers))
		for k, v := range b.Headers {
			r, err := ResolveEnv(v)
			if err != nil {
				return nil, fmt.Errorf("header %s: %w", k, err)
			}
			c.Headers[k] = r
		}
	}
	if b.Auth != nil {
		a := *b.Auth
		var err error
		if a.Token, err = ResolveEnv(a.Token); err != nil {
			return nil, fmt.Errorf("auth.token: %w", err)
		}
		if a.ClientID, err = ResolveEnv(a.ClientID); err != nil {
			return nil, fmt.Errorf("auth.client_id: %w", err)
		}
		if a.ClientSecret, err = ResolveEnv(a.ClientSecret); err != nil {
			return nil, fmt.Errorf("auth.client_secret: %w", err)
		}
		c.Auth = &a
	}
	return &c, nil
}

func (f *File) validate() error {
	if f.Mode != ModeStateless && f.Mode != ModeStateful {
		return fmt.Errorf("mode must be %q or %q", ModeStateless, ModeStateful)
	}
	// env: references must resolve — a missing var fails closed at load,
	// not at first call.
	if _, err := f.ResolvedToken(); err != nil {
		return fmt.Errorf("token: %w", err)
	}
	if f.CallTimeout != "" {
		if _, err := parseTimeout(f.CallTimeout); err != nil {
			return fmt.Errorf("call_timeout: %w", err)
		}
	}
	for name, b := range f.Backends {
		if name == "toolhost" {
			return fmt.Errorf("backend name %q is reserved for the gateway's own tools", name)
		}
		if err := namespace.ValidateNamespace(name); err != nil {
			return fmt.Errorf("backend name %q: %w", name, err)
		}
		if b == nil {
			return fmt.Errorf("backend %q: empty block", name)
		}
		if _, err := b.Resolved(); err != nil {
			return fmt.Errorf("backend %q: %w", name, err)
		}
		switch b.Transport {
		case "stdio":
			if b.Command == "" {
				return fmt.Errorf("backend %q: stdio transport requires command", name)
			}
		case "http", "streamable_http", "sse":
			if b.URL == "" {
				return fmt.Errorf("backend %q: %s transport requires url", name, b.Transport)
			}
		default:
			return fmt.Errorf("backend %q: transport must be %q, %q or %q", name, "stdio", "http", "sse")
		}
		if b.CallTimeout != "" {
			if _, err := parseTimeout(b.CallTimeout); err != nil {
				return fmt.Errorf("backend %q call_timeout: %w", name, err)
			}
		}
		if err := b.validateAuth(name); err != nil {
			return err
		}
	}
	for _, q := range f.Approved {
		if _, _, err := namespace.Split(q); err != nil {
			return fmt.Errorf("approved name %q: %w", q, err)
		}
	}
	if f.Enabled != nil {
		for _, q := range *f.Enabled {
			if _, _, err := namespace.Split(q); err != nil {
				return fmt.Errorf("enabled name %q: %w", q, err)
			}
		}
	}
	return nil
}

// validateAuth enforces the auth block's shape. Auth is meaningless on
// stdio — a subprocess gets its credentials through env, not headers.
func (b *Backend) validateAuth(name string) error {
	if b.Auth == nil || b.Auth.Type == "" || b.Auth.Type == "none" {
		return nil
	}
	if b.Transport == "stdio" {
		return fmt.Errorf("backend %q: auth is only valid on http/sse transports — use env for stdio", name)
	}
	switch b.Auth.Type {
	case "bearer":
		if b.Auth.Token == "" {
			return fmt.Errorf("backend %q: auth.type bearer requires token", name)
		}
	case "client_credentials":
		if b.Auth.ClientID == "" || b.Auth.ClientSecret == "" {
			return fmt.Errorf("backend %q: client_credentials requires client_id and client_secret", name)
		}
	case "oauth":
		// All fields optional: no client_id means DCR; defaults apply elsewhere.
	default:
		return fmt.Errorf("backend %q: auth.type must be bearer, client_credentials, or oauth", name)
	}
	return nil
}

// Save writes the config back atomically (0600 — it carries the token) —
// tmp+rename so the serve watcher never reads a torn file.
func (f *File) Save(path string) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ApprovedSet is the lookup form Resolve consumes.
func (f *File) ApprovedSet() map[string]bool {
	set := make(map[string]bool, len(f.Approved))
	for _, q := range f.Approved {
		set[q] = true
	}
	return set
}

func (f *File) IsApproved(qualified string) bool {
	return f.ApprovedSet()[qualified]
}

// EnabledSet returns the allowlist and whether one is configured —
// (nil, false) means "all approved tools are enabled".
func (f *File) EnabledSet() (map[string]bool, bool) {
	if f.Enabled == nil {
		return nil, false
	}
	set := make(map[string]bool, len(*f.Enabled))
	for _, q := range *f.Enabled {
		set[q] = true
	}
	return set, true
}

// IsEnabled reports whether a qualified tool is on the live surface:
// enabled (no list, or listed) AND approved.
func (f *File) IsEnabled(qualified string) bool {
	if !f.IsApproved(qualified) {
		return false
	}
	set, has := f.EnabledSet()
	return !has || set[qualified]
}

// Enable adds qualified names to the live surface. No-op (nil) while no
// allowlist exists — without one, every approved tool is already enabled.
// Enabled names must be approved first; enabling is never a backdoor
// around approval.
func (f *File) Enable(names ...string) ([]string, error) {
	if f.Enabled == nil {
		return nil, nil
	}
	approved := f.ApprovedSet()
	set, _ := f.EnabledSet()
	var added []string
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if !approved[n] {
			return nil, fmt.Errorf("%q is not approved — run: toolhost approve %s", n, n)
		}
		if set[n] {
			continue
		}
		set[n] = true
		added = append(added, n)
	}
	if len(added) > 0 {
		list := append(*f.Enabled, added...)
		sort.Strings(list)
		f.Enabled = &list
	}
	return added, nil
}

// EnableOnly replaces the live surface with exactly the given names —
// the "I need just these tools" form. All must be approved.
func (f *File) EnableOnly(names ...string) ([]string, error) {
	approved := f.ApprovedSet()
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if !approved[n] {
			return nil, fmt.Errorf("%q is not approved — run: toolhost approve %s", n, n)
		}
	}
	list := append([]string{}, names...)
	sort.Strings(list)
	f.Enabled = &list
	return names, nil
}

// EnableAll clears the allowlist — every approved tool is enabled again.
func (f *File) EnableAll() {
	f.Enabled = nil
}

// Disable hides approved tools without un-approving them. The first
// disable materializes the allowlist as approved ∖ names — after that,
// the live surface is always explicit.
func (f *File) Disable(names ...string) ([]string, error) {
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
	}
	if f.Enabled == nil {
		set := f.ApprovedSet()
		for _, n := range names {
			delete(set, n)
		}
		list := make([]string, 0, len(set))
		for q := range set {
			list = append(list, q)
		}
		sort.Strings(list)
		f.Enabled = &list
		return names, nil
	}
	set, _ := f.EnabledSet()
	var removed []string
	for _, n := range names {
		if set[n] {
			delete(set, n)
			removed = append(removed, n)
		}
	}
	if len(removed) > 0 {
		list := make([]string, 0, len(set))
		for q := range set {
			list = append(list, q)
		}
		sort.Strings(list)
		f.Enabled = &list
	}
	return removed, nil
}

// parseTimeout validates a duration string — must parse and be positive.
func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("not a duration (want e.g. \"60s\", \"2m\"): %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive, got %q", s)
	}
	return d, nil
}

// RequestTools records agent approval requests: qualified names, deduped,
// newest-last. Requesting is not approval — a human still has to approve.
func (f *File) RequestTools(reason string, names ...string) ([]string, error) {
	have := make(map[string]bool, len(f.Requested))
	for _, r := range f.Requested {
		have[r.Name] = true
	}
	approved := f.ApprovedSet()
	var added []string
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if approved[n] {
			return nil, fmt.Errorf("%q is already approved — enable it with toolhost__enable", n)
		}
		if have[n] {
			continue
		}
		f.Requested = append(f.Requested, Request{Name: n, Reason: reason, At: time.Now().UTC()})
		have[n] = true
		added = append(added, n)
	}
	return added, nil
}

// RequestedSet is the lookup form — name → pending request.
func (f *File) RequestedSet() map[string]bool {
	set := make(map[string]bool, len(f.Requested))
	for _, r := range f.Requested {
		set[r.Name] = true
	}
	return set
}

// Approve adds qualified names; returns those actually added. Approved
// names drop out of `requested` — the request has been answered.
func (f *File) Approve(names ...string) ([]string, error) {
	set := f.ApprovedSet()
	var added []string
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if set[n] {
			continue
		}
		set[n] = true
		added = append(added, n)
	}
	if len(added) > 0 {
		f.Approved = append(f.Approved, added...)
		sort.Strings(f.Approved)
	}
	// Answered requests leave the queue — approval is the response.
	if len(f.Requested) > 0 {
		kept := f.Requested[:0]
		for _, r := range f.Requested {
			if !set[r.Name] {
				kept = append(kept, r)
			}
		}
		f.Requested = kept
	}
	return added, nil
}

// Revoke removes qualified names; returns those actually removed.
func (f *File) Revoke(names ...string) []string {
	set := f.ApprovedSet()
	removed := map[string]bool{}
	for _, n := range names {
		if set[n] {
			delete(set, n)
			removed[n] = true
		}
	}
	if len(removed) == 0 {
		return nil
	}
	kept := f.Approved[:0]
	for _, q := range f.Approved {
		if !removed[q] {
			kept = append(kept, q)
		}
	}
	f.Approved = kept
	var out []string
	for n := range removed {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
