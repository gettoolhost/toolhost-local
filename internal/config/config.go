// Package config is the driven adapter for the JSON config file — the only
// state toolhost keeps. Approval is a list of qualified names in this file;
// `toolhost approve` edits it, `serve` reads it at boot.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// EnvAllowlist names host environment variables forwarded to the
	// subprocess beyond the safe baseline (PATH, HOME, ...). Credentials
	// should reach a backend by being named — here, or via env values
	// holding "env:" references — never by inheriting everything.
	EnvAllowlist []string `json:"env_allowlist,omitempty"`
	// EnvInherit forwards the entire host environment: every credential in
	// the gateway's shell reaches the backend. The least-safe option —
	// prefer Env + EnvAllowlist.
	EnvInherit bool `json:"env_inherit,omitempty"`

	// http/sse transports:
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Auth    *Auth             `json:"auth,omitempty"`

	// Passthrough marks this upstream as a federated toolhost gateway: its
	// tool names arrive already qualified ("cbm__search_graph") and are
	// trusted as-is instead of being re-namespaced. Its own gateway__*
	// meta-tools are dropped — the outer gateway has its own control plane.
	Passthrough bool `json:"passthrough,omitempty"`

	// Disabled parks the backend: the entry stays, nothing dials, and its
	// tools leave the surface. `toolhost mcp enable` reconnects it. The
	// tools' approvals stay pinned — parking is not revocation.
	Disabled bool `json:"disabled,omitempty"`

	// Cwd is the working directory a stdio backend spawns in — unset means
	// inherit the gateway process's directory.
	Cwd string `json:"cwd,omitempty"`

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
	// tools/list, tools/call each stand alone; they cannot receive
	// tools/list_changed without a held session. Modern clients can subscribe
	// through subscriptions/listen. "stateful" is the opt-in compat mode for
	// older clients that must hold Mcp-Session-Id.
	Mode string `json:"mode,omitempty"`
	// Approved is the qualified-name list ("backend__tool") that Resolve
	// intersects with discovery. discovered ≠ approved: everything not on
	// this list is invisible and uncallable.
	Approved []string `json:"approved"`
	// ApprovedSchemas binds an approval to the contract reviewed at
	// approve time: qualified name → "sha256:…" of the tool's canonical
	// input+output schema. If the upstream's schema changes, the approval
	// lapses — the tool drops off the surface until a human re-approves
	// (which pins the new schema). Approved names with no entry here are
	// unbound: approvals made before schema binding keep working until
	// re-approved.
	ApprovedSchemas map[string]string `json:"approved_schemas,omitempty"`
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

	// AuditMaxMB caps the audit log size in MiB — past it the log rotates
	// to <audit_log>.1 (one generation kept; evidence is append-mostly and
	// recent history is what introspection needs). Absent or non-positive
	// means the default.
	AuditMaxMB int `json:"audit_max_mb,omitempty"`
}

// Request is one agent-filed petition. Kind "tool" asks approval for a
// discovered qualified name; kind "server" proposes a whole new backend —
// the human materializes it into config with `toolhost requests --accept`.
// A denied petition stays on record so the agent sees the verdict rather
// than re-asking blindly; a human clears it by re-requesting or accepting.
type Request struct {
	Kind   string    `json:"kind"` // "tool" | "server"
	Name   string    `json:"name"` // qualified tool name, or proposed backend name
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`

	// server petitions — the proposed backend spec:
	Transport string   `json:"transport,omitempty"` // "stdio" | "http"
	URL       string   `json:"url,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`

	// the human's verdict:
	Denied     bool   `json:"denied,omitempty"`
	DenyReason string `json:"deny_reason,omitempty"`
}

// Request kinds.
const (
	RequestTool   = "tool"
	RequestServer = "server"
)

const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultAuditLog = "toolhost_audit.jsonl"

	// DefaultAuditMaxMB is the audit log's rotation cap.
	DefaultAuditMaxMB = 10

	// ModeStateless is the default serving mode — sessionless Streamable
	// HTTP. ModeStateful opts into session-bearing mode for older clients
	// that need held sessions for pushed notifications/tools/list_changed.
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

// ConfigHome is toolhost's canonical home — $TOOLHOST_HOME if set, else
// ~/.toolhost. The user's global config lives here along with every
// environment's state: credentials, audit logs, and request queues never
// live in project folders.
func ConfigHome() string {
	if x := os.Getenv("TOOLHOST_HOME"); x != "" {
		return x
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".toolhost" // last resort: cwd
	}
	return filepath.Join(home, ".toolhost")
}

// projectsDir holds one subdirectory per project environment, keyed by the
// folder's absolute path.
func projectsDir() string {
	return filepath.Join(ConfigHome(), "projects")
}

// ProjectKey turns a directory's absolute path into a stable, filesystem-
// safe environment id: basename slug + short path hash, so two same-named
// projects in different trees never collide.
func ProjectKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	sum := sha256.Sum256([]byte(abs))
	var b strings.Builder
	for _, r := range strings.ToLower(filepath.Base(abs)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "env"
	}
	return fmt.Sprintf("%s-%s", slug, hex.EncodeToString(sum[:4]))
}

// ProjectConfigPath is the private environment for a folder — keyed under
// the user's home, never written into the folder itself.
func ProjectConfigPath(dir string) string {
	return filepath.Join(projectsDir(), ProjectKey(dir), "toolhost.json")
}

// StateDir is where an environment's runtime state lives — tokens, audit
// log, request queue. Global config and private envs keep state beside
// themselves (inside the home); a config anywhere else (a committed
// ./toolhost.json, an explicit -c path) gets a keyed state dir so the
// folder holding the config stays pure declaration.
func StateDir(cfgPath string) string {
	dir, err := filepath.Abs(filepath.Dir(cfgPath))
	if err != nil {
		dir = filepath.Dir(cfgPath)
	}
	home, err := filepath.Abs(ConfigHome())
	if err != nil {
		home = ConfigHome()
	}
	if dir == home || strings.HasPrefix(dir, filepath.Join(home, "projects")+string(filepath.Separator)) {
		return dir
	}
	return filepath.Join(projectsDir(), ProjectKey(dir))
}

// SharedConfigPath is the committed environment — a hidden, namespaced
// path so an agent working in the folder never mistakes it for project
// content. Opt-in only, written by `init --shared`.
const SharedConfigPath = ".toolhost/toolhost.json"

// DefaultPath resolves which config a command reads when -c isn't given,
// nearest scope first: a private environment keyed for this folder, then
// the committed ./.toolhost/toolhost.json, then the global home config.
// Mirrors the agents' own local > project > user precedence.
func DefaultPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if cwd, err := os.Getwd(); err == nil {
		if p := ProjectConfigPath(cwd); fileExists(p) {
			return p
		}
	}
	if fileExists(SharedConfigPath) {
		return SharedConfigPath
	}
	return filepath.Join(ConfigHome(), "toolhost.json")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// RequestsFile is the env's agent-request queue — runtime state, kept out
// of the config so a committed toolhost.json is never mutated by agents.
const RequestsFile = "toolhost_requests.json"

// RequestsPath is the queue file for the environment cfgPath resolves to.
func RequestsPath(cfgPath string) string {
	return filepath.Join(StateDir(cfgPath), RequestsFile)
}

// LoadRequests reads the pending agent-request queue from the env's
// state dir — never the config file.
func LoadRequests(cfgPath string) ([]Request, error) {
	raw, err := os.ReadFile(RequestsPath(cfgPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var reqs []Request
	if err := json.Unmarshal(raw, &reqs); err != nil {
		return nil, fmt.Errorf("parse %s: %w", RequestsPath(cfgPath), err)
	}
	return reqs, nil
}

// SaveRequests writes the queue file (0600) into the env's state dir.
func SaveRequests(cfgPath string, reqs []Request) error {
	p := RequestsPath(cfgPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(reqs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(raw, '\n'), 0o600)
}

// AppendRequests queues qualified tool names (deduped, newest-last) in
// the env's request file — what gateway__request records. Approved names
// are refused: the request is already answered. A name a human denied is
// refused too — denial is a verdict, not a speed bump.
func AppendRequests(cfgPath string, approved map[string]bool, reason string, names ...string) ([]string, error) {
	reqs, err := LoadRequests(cfgPath)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(reqs))
	denied := map[string]string{}
	for _, r := range reqs {
		have[r.Name] = true
		if r.Denied {
			denied[r.Name] = r.DenyReason
		}
	}
	var added []string
	for _, n := range names {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if approved[n] {
			return nil, fmt.Errorf("%q is already approved — enable it with gateway__enable", n)
		}
		if why, wasDenied := denied[n]; wasDenied {
			if why == "" {
				why = "no reason given"
			}
			return nil, fmt.Errorf("%q was denied by the human — %s", n, why)
		}
		if have[n] {
			continue
		}
		reqs = append(reqs, Request{Kind: RequestTool, Name: n, Reason: reason, At: time.Now().UTC()})
		have[n] = true
		added = append(added, n)
	}
	if len(added) == 0 {
		return added, nil
	}
	return added, SaveRequests(cfgPath, reqs)
}

// AppendServerRequest queues a proposed backend — the agent supplies the
// spec it can see (name, transport, url or command+args); the human
// materializes it into config on --accept. Name collisions with existing
// backends or denied proposals refuse.
func AppendServerRequest(cfgPath string, f *File, r Request) error {
	if err := f.validateBackend(r.Name, &Backend{
		Transport: r.Transport, Command: r.Command, Args: r.Args, URL: r.URL,
	}); err != nil {
		return err
	}
	if _, exists := f.Backends[r.Name]; exists {
		return fmt.Errorf("backend %q is already configured — its tools are discoverable, request tools on it instead", r.Name)
	}
	reqs, err := LoadRequests(cfgPath)
	if err != nil {
		return err
	}
	for _, q := range reqs {
		if q.Name != r.Name || q.Kind != RequestServer {
			continue
		}
		if q.Denied {
			why := q.DenyReason
			if why == "" {
				why = "no reason given"
			}
			return fmt.Errorf("server %q was denied by the human — %s", r.Name, why)
		}
		return nil // already queued — a re-ask adds nothing
	}
	r.Kind = RequestServer
	r.At = time.Now().UTC()
	reqs = append(reqs, r)
	return SaveRequests(cfgPath, reqs)
}

// DenyRequest marks one queued petition denied — the verdict stays on
// record so the agent sees it rather than re-asking.
func DenyRequest(cfgPath, name, reason string) error {
	reqs, err := LoadRequests(cfgPath)
	if err != nil {
		return err
	}
	for i, r := range reqs {
		if r.Name == name && !r.Denied {
			reqs[i].Denied = true
			reqs[i].DenyReason = reason
			return SaveRequests(cfgPath, reqs)
		}
	}
	return fmt.Errorf("no pending request %q", name)
}

// RemoveRequest drops a queue entry — the cleanup after --accept or an
// explicit dismissal.
func RemoveRequest(cfgPath, name string) error {
	reqs, err := LoadRequests(cfgPath)
	if err != nil {
		return err
	}
	kept := reqs[:0]
	for _, r := range reqs {
		if r.Name != name {
			kept = append(kept, r)
		}
	}
	return SaveRequests(cfgPath, kept)
}

// PendingRequests filters the queue to undecided petitions.
func PendingRequests(reqs []Request) []Request {
	var out []Request
	for _, r := range reqs {
		if !r.Denied {
			out = append(out, r)
		}
	}
	return out
}

// DeniedSet is the lookup form — name → deny reason, for the catalog's
// denied mark.
func DeniedSet(reqs []Request) map[string]string {
	set := map[string]string{}
	for _, r := range reqs {
		if r.Denied {
			set[r.Name] = r.DenyReason
		}
	}
	return set
}

// RemoveAnsweredRequests drops queue entries whose names are now approved
// — an approval is the answer to the request.
func RemoveAnsweredRequests(cfgPath string, approved map[string]bool) {
	reqs, err := LoadRequests(cfgPath)
	if err != nil || len(reqs) == 0 {
		return
	}
	kept := reqs[:0]
	for _, r := range reqs {
		if !approved[r.Name] {
			kept = append(kept, r)
		}
	}
	if len(kept) != len(reqs) {
		_ = SaveRequests(cfgPath, kept)
	}
}

// RequestSet is the lookup form — name → pending (undecided) request.
func RequestSet(reqs []Request) map[string]bool {
	set := make(map[string]bool, len(reqs))
	for _, r := range reqs {
		if !r.Denied {
			set[r.Name] = true
		}
	}
	return set
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
	if f.AuditMaxMB <= 0 {
		f.AuditMaxMB = DefaultAuditMaxMB
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
	if b.Env != nil {
		c.Env = make(map[string]string, len(b.Env))
		for k, v := range b.Env {
			r, err := ResolveEnv(v)
			if err != nil {
				return nil, fmt.Errorf("env %s: %w", k, err)
			}
			c.Env[k] = r
		}
	}
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
	// The gateway token resolves lazily at the boundaries that need it
	// (HTTP serve, attach, doctor) — a committed project config may carry
	// an env: reference whose var only exists where serving happens.
	if f.CallTimeout != "" {
		if _, err := parseTimeout(f.CallTimeout); err != nil {
			return fmt.Errorf("call_timeout: %w", err)
		}
	}
	// Bound the rotation cap — int64(AuditMaxMB)<<20 overflows into a
	// negative maxBytes on absurd values, silently disabling rotation.
	if f.AuditMaxMB > 1<<20 {
		return fmt.Errorf("audit_max_mb %d exceeds the 1 TiB cap", f.AuditMaxMB)
	}
	for name, b := range f.Backends {
		if err := f.validateBackend(name, b); err != nil {
			return err
		}
	}
	for _, q := range f.Approved {
		if _, _, err := namespace.Split(q); err != nil {
			return fmt.Errorf("approved name %q: %w", q, err)
		}
	}
	for q, h := range f.ApprovedSchemas {
		if _, _, err := namespace.Split(q); err != nil {
			return fmt.Errorf("approved_schemas key %q: %w", q, err)
		}
		if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
			return fmt.Errorf("approved_schemas[%q]: want \"sha256:<64 hex>\", got %q", q, h)
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

// validateBackend checks one backend entry — the same rules f.validate
// applies to the whole map, exported for single-entry writes like
// `toolhost mcp add`.
func (f *File) validateBackend(name string, b *Backend) error {
	if name == "toolhost" || name == "gateway" {
		return fmt.Errorf("backend name %q is reserved for the gateway's own control plane", name)
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
	if b.Transport != "stdio" && (len(b.Env) > 0 || len(b.EnvAllowlist) > 0 || b.EnvInherit) {
		return fmt.Errorf("backend %q: env, env_allowlist and env_inherit only apply to stdio transports", name)
	}
	if b.EnvInherit && len(b.EnvAllowlist) > 0 {
		return fmt.Errorf("backend %q: env_inherit already passes every variable — env_allowlist is redundant", name)
	}
	if b.Cwd != "" && b.Transport != "stdio" {
		return fmt.Errorf("backend %q: cwd only applies to stdio transports", name)
	}
	if b.CallTimeout != "" {
		if _, err := parseTimeout(b.CallTimeout); err != nil {
			return fmt.Errorf("backend %q call_timeout: %w", name, err)
		}
	}
	return b.validateAuth(name)
}

// SetBackend inserts or replaces one backend after validation — the
// `mcp add`/`requests --accept` write path. Callers Save afterwards.
func (f *File) SetBackend(name string, b *Backend) error {
	if err := f.validateBackend(name, b); err != nil {
		return err
	}
	f.Backends[name] = b
	return nil
}

// RemoveBackend drops a backend entry and reports whether anything
// changed. Approval cleanup is the caller's decision — pruning policy
// silently is how envelopes leak.
func (f *File) RemoveBackend(name string) bool {
	if _, ok := f.Backends[name]; !ok {
		return false
	}
	delete(f.Backends, name)
	return true
}

// BackendToolsApproved returns the approved qualified names belonging to
// a backend's namespace — the cascade check `mcp remove` runs first.
func (f *File) BackendToolsApproved(name string) []string {
	var out []string
	for _, q := range f.Approved {
		if ns, _, err := namespace.Split(q); err == nil && ns == name {
			out = append(out, q)
		}
	}
	return out
}

// PruneBackendTools drops every approved/enabled name in a backend's
// namespace — the explicit --prune-approvals form of mcp remove.
func (f *File) PruneBackendTools(name string) []string {
	var doomed []string
	for _, q := range f.Approved {
		if ns, _, err := namespace.Split(q); err == nil && ns == name {
			doomed = append(doomed, q)
		}
	}
	if f.Enabled != nil {
		for _, q := range *f.Enabled {
			if ns, _, err := namespace.Split(q); err == nil && ns == name {
				doomed = append(doomed, q)
			}
		}
	}
	removed := f.Revoke(doomed...)
	if f.Enabled != nil {
		var kept []string
		for _, q := range *f.Enabled {
			if ns, _, err := namespace.Split(q); err != nil || ns != name {
				kept = append(kept, q)
			}
		}
		list := kept
		f.Enabled = &list
	}
	return removed
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

// Approve adds qualified names bound to their schema pins (name →
// SchemaHash of the contract reviewed; "" = unbound). Returns the names
// that changed: newly approved, or already-approved with a refreshed pin —
// the latter is how a drifted schema gets re-affirmed, and it MUST count
// as a change or the caller skips saving the pin. Approved names drop out
// of `requested` — the request has been answered.
func (f *File) Approve(pins map[string]string) ([]string, error) {
	// Validate the whole batch first — a bad pin late in the map must not
	// leave earlier names half-applied in memory.
	for n, hash := range pins {
		if _, _, err := namespace.Split(n); err != nil {
			return nil, fmt.Errorf("%q is not a qualified name (want backend__tool): %w", n, err)
		}
		if hash != "" && (!strings.HasPrefix(hash, "sha256:") || len(hash) != len("sha256:")+64) {
			return nil, fmt.Errorf("%q: schema pin must be \"sha256:<64 hex>\", got %q", n, hash)
		}
	}
	set := f.ApprovedSet()
	var added, changed []string
	for n, hash := range pins {
		changedHere := false
		if hash != "" && f.ApprovedSchemas[n] != hash {
			if f.ApprovedSchemas == nil {
				f.ApprovedSchemas = map[string]string{}
			}
			f.ApprovedSchemas[n] = hash
			changedHere = true
		}
		if set[n] {
			if changedHere {
				changed = append(changed, n)
			}
			continue
		}
		set[n] = true
		added = append(added, n)
		changed = append(changed, n)
	}
	if len(added) > 0 {
		f.Approved = append(f.Approved, added...)
		sort.Strings(f.Approved)
	}
	sort.Strings(changed)
	return changed, nil
}

// Revoke removes qualified names and their schema pins; returns those
// actually removed.
func (f *File) Revoke(names ...string) []string {
	set := f.ApprovedSet()
	removed := map[string]bool{}
	for _, n := range names {
		if set[n] {
			delete(set, n)
			removed[n] = true
		}
		delete(f.ApprovedSchemas, n)
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
