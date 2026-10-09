package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/namespace"
)

// ResolvedTool is one upstream tool that is BOTH visible and callable under
// its qualified name. The pair cannot come apart: the front door registers
// exactly these, and dispatch closes over Upstream+Origin directly.
type ResolvedTool struct {
	Qualified string    // "fs__read_file" — the wire name
	Origin    string    // "read_file" — the upstream's own name
	Upstream  Upstream  // session to dispatch to
	Tool      *mcp.Tool // namespaced copy registered on the front door
}

// SkippedTool is a discovered tool that cannot be exposed — with the reason.
// Skipping is per-tool: one bad upstream name must never darken a backend.
type SkippedTool struct {
	Backend   string
	Tool      string
	Qualified string
	Reason    string
}

// Resolution is the output of the one place visibility is decided.
type Resolution struct {
	Tools   []ResolvedTool // qualified-name sorted
	Skipped []SkippedTool
	// Drifted holds approved tools whose schema no longer matches the hash
	// recorded at approve time — approval is bound to the contract that was
	// reviewed, so drift lapses it until a human re-approves.
	Drifted []SkippedTool
}

// Passthrough marks an upstream whose tool names are already qualified —
// a federated toolhost gateway. Resolve trusts the names instead of
// re-namespacing.
type Passthrough interface {
	Passthrough() bool
}

// Qualify is the one name-mapping rule shared by Resolve and the meta
// catalog: passthrough upstreams keep their (already qualified) names —
// except the inner gateway's own toolhost__* control plane, which is
// dropped; everything else is namespaced backend__tool.
func Qualify(up Upstream, name string) (string, error) {
	if p, ok := up.(Passthrough); ok && p.Passthrough() {
		if strings.HasPrefix(name, "toolhost"+namespace.Separator) {
			return "", fmt.Errorf("toolhost__* is the inner gateway's control plane — not forwarded")
		}
		if _, _, err := namespace.Split(name); err != nil {
			return "", fmt.Errorf("passthrough upstream tool %q is not a qualified name: %w", name, err)
		}
		return name, nil
	}
	return namespace.Join(up.Namespace(), name)
}

// SchemaHash fingerprints the contract a tool is approved against: input
// and output schemas as canonical JSON. The unmarshal/remarshal round-trip
// normalizes whitespace and map order, so transport formatting can't
// false-positive as drift — only a real schema change lapses an approval.
func SchemaHash(t *mcp.Tool) string {
	canon := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			return v
		}
		var out any
		if json.Unmarshal(raw, &out) != nil {
			return v
		}
		return out
	}
	raw, _ := json.Marshal(map[string]any{"input": canon(t.InputSchema), "output": canon(t.OutputSchema)})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Resolve answers "what can be seen and called" — the ONLY place that
// question is answered. A tool is exposed iff it is discovered AND approved
// AND enabled: discovered ≠ approved ≠ enabled. A nil enabled set means
// "no allowlist configured" — every approved tool is enabled. A non-nil
// enabled set intersects: the live surface is enabled ∩ approved.
//
// schemas binds approvals to the contract reviewed at approve time:
// qualified name → SchemaHash. An approved tool whose hash mismatches is
// drifted — it drops off the surface (audited) until re-approved. An
// approved name with no recorded hash is unbound (pre-binding approvals)
// and enforced as before.
//
// A duplicate qualified name is a hard error: two upstreams claiming one
// wire name is a routing ambiguity, and ambiguity denies.
func Resolve(upstreams []Upstream, approved, enabled map[string]bool, schemas map[string]string) (*Resolution, error) {
	res := &Resolution{}
	seen := map[string]string{}
	for _, up := range upstreams {
		for _, tool := range up.Tools() {
			qualified, err := Qualify(up, tool.Name)
			if err != nil {
				res.Skipped = append(res.Skipped, SkippedTool{
					Backend: up.Namespace(), Tool: tool.Name, Qualified: qualified, Reason: err.Error(),
				})
				continue
			}
			if other, dup := seen[qualified]; dup {
				return nil, fmt.Errorf("qualified name %q claimed by both %q and %q — refusing to route ambiguously",
					qualified, other, up.Namespace())
			}
			seen[qualified] = up.Namespace()
			if !approved[qualified] {
				continue
			}
			if want := schemas[qualified]; want != "" && want != SchemaHash(tool) {
				res.Drifted = append(res.Drifted, SkippedTool{
					Backend: up.Namespace(), Tool: tool.Name, Qualified: qualified,
					Reason: "schema changed since approval — re-approve to pin the new contract",
				})
				continue
			}
			if enabled != nil && !enabled[qualified] {
				continue
			}
			namespaced := *tool
			namespaced.Name = qualified
			res.Tools = append(res.Tools, ResolvedTool{
				Qualified: qualified,
				Origin:    tool.Name,
				Upstream:  up,
				Tool:      &namespaced,
			})
		}
	}
	sort.Slice(res.Tools, func(i, j int) bool { return res.Tools[i].Qualified < res.Tools[j].Qualified })
	return res, nil
}

// IsQualified reports whether s parses as a qualified tool name.
func IsQualified(s string) bool {
	_, _, err := namespace.Split(s)
	return err == nil
}

// ErrNotApproved is the fail-closed default for any name Resolve did not
// produce — including never-discovered and malformed names.
var ErrNotApproved = errors.New("tool is not approved or does not exist")
