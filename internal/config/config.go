// Package config is the driven adapter for the JSON config file — the only
// state toolhost keeps. Approval is a list of qualified names in this file;
// `toolhost approve` edits it, `serve` reads it at boot.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"toolhost/internal/namespace"
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
	// Approved is the qualified-name list ("backend__tool") that Resolve
	// intersects with discovery. discovered ≠ approved: everything not on
	// this list is invisible and uncallable.
	Approved []string `json:"approved"`
}

const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultAuditLog = "toolhost_audit.jsonl"
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
	if f.AuditLog == "" {
		f.AuditLog = DefaultAuditLog
	}
	if f.Backends == nil {
		f.Backends = map[string]*Backend{}
	}
	if f.Approved == nil {
		f.Approved = []string{}
	}
}

func (f *File) validate() error {
	for name, b := range f.Backends {
		if err := namespace.ValidateNamespace(name); err != nil {
			return fmt.Errorf("backend name %q: %w", name, err)
		}
		if b == nil {
			return fmt.Errorf("backend %q: empty block", name)
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
		if err := b.validateAuth(name); err != nil {
			return err
		}
	}
	for _, q := range f.Approved {
		if _, _, err := namespace.Split(q); err != nil {
			return fmt.Errorf("approved name %q: %w", q, err)
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

// Save writes the config back atomically (0600 — it carries the token).
func (f *File) Save(path string) error {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
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

// Approve adds qualified names; returns those actually added.
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
