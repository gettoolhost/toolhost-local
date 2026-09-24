package core

import (
	"errors"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"toolhost/internal/namespace"
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
	Backend string
	Tool    string
	Reason  string
}

// Resolution is the output of the one place visibility is decided.
type Resolution struct {
	Tools   []ResolvedTool // qualified-name sorted
	Skipped []SkippedTool
}

// Resolve answers "what can be seen and called" — the ONLY place that
// question is answered. A tool is exposed iff it is discovered AND approved:
// discovered-but-unapproved is invisible and (structurally) uncallable.
//
// A duplicate qualified name is a hard error: two upstreams claiming one
// wire name is a routing ambiguity, and ambiguity denies.
func Resolve(upstreams []Upstream, approved map[string]bool) (*Resolution, error) {
	res := &Resolution{}
	seen := map[string]string{}
	for _, up := range upstreams {
		for _, tool := range up.Tools() {
			qualified, err := namespace.Join(up.Namespace(), tool.Name)
			if err != nil {
				res.Skipped = append(res.Skipped, SkippedTool{
					Backend: up.Namespace(), Tool: tool.Name, Reason: err.Error(),
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
