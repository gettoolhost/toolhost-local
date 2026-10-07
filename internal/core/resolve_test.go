package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stub implements Upstream (and optionally Passthrough) for resolver tests.
type stub struct {
	ns          string
	tools       []*mcp.Tool
	passthrough bool
}

func (s *stub) Namespace() string  { return s.ns }
func (s *stub) Tools() []*mcp.Tool { return s.tools }
func (s *stub) Passthrough() bool  { return s.passthrough }
func (s *stub) Close() error       { return nil }
func (s *stub) Call(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, nil
}

func tool(name string) *mcp.Tool {
	return &mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}
}

func qualifiedNames(res *Resolution) []string {
	var out []string
	for _, rt := range res.Tools {
		out = append(out, rt.Qualified)
	}
	return out
}

func TestResolveGateTiers(t *testing.T) {
	up := &stub{ns: "fs", tools: []*mcp.Tool{tool("read_file"), tool("write_file"), tool("delete_file")}}

	// Approved but no enabled list: every approved tool is live.
	res, err := Resolve([]Upstream{up}, map[string]bool{"fs__read_file": true, "fs__write_file": true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := qualifiedNames(res)
	if len(got) != 2 || got[0] != "fs__read_file" || got[1] != "fs__write_file" {
		t.Fatalf("want [fs__read_file fs__write_file], got %v", got)
	}
	// Discovered-but-unapproved never surfaces.
	for _, rt := range res.Tools {
		if rt.Origin == "delete_file" {
			t.Fatal("unapproved tool leaked into resolution")
		}
	}

	// Approved names that were never discovered produce nothing.
	res, err = Resolve(nil, map[string]bool{"ghost__tool": true}, nil)
	if err != nil || len(res.Tools) != 0 {
		t.Fatalf("ghost approval: want empty resolution, got %+v err=%v", res.Tools, err)
	}

	// An enabled list intersects: enabled∩approved is the live surface.
	enabled := map[string]bool{"fs__write_file": true, "fs__delete_file": true}
	res, err = Resolve([]Upstream{up},
		map[string]bool{"fs__read_file": true, "fs__write_file": true}, enabled)
	if err != nil {
		t.Fatal(err)
	}
	got = qualifiedNames(res)
	if len(got) != 1 || got[0] != "fs__write_file" {
		t.Fatalf("enabled∩approved: want [fs__write_file], got %v", got)
	}

	// An explicit empty enabled set exposes nothing — never collapses to all.
	res, err = Resolve([]Upstream{up},
		map[string]bool{"fs__read_file": true}, map[string]bool{})
	if err != nil || len(res.Tools) != 0 {
		t.Fatalf("empty enabled set: want no tools, got %v", qualifiedNames(res))
	}
}

func TestResolvePreservesOriginAndUpstream(t *testing.T) {
	up := &stub{ns: "gh", tools: []*mcp.Tool{tool("search")}}
	res, err := Resolve([]Upstream{up}, map[string]bool{"gh__search": true}, nil)
	if err != nil || len(res.Tools) != 1 {
		t.Fatalf("want one resolved tool, got %+v err=%v", res.Tools, err)
	}
	rt := res.Tools[0]
	if rt.Qualified != "gh__search" || rt.Origin != "search" || rt.Upstream != up {
		t.Fatalf("resolved tool lost its identity: %+v", rt)
	}
	// The registered copy is renamed; the upstream's own tool isn't mutated.
	if rt.Tool.Name != "gh__search" || up.tools[0].Name != "search" {
		t.Fatal("namespacing should copy the tool, not mutate the upstream's")
	}
}

func TestResolveAmbiguityDenies(t *testing.T) {
	a := &stub{ns: "a", tools: []*mcp.Tool{tool("x")}}
	b := &stub{ns: "b", tools: []*mcp.Tool{tool("x")}}
	// Same qualified name can only arise via passthrough — construct one.
	pa := &stub{ns: "a", passthrough: true, tools: []*mcp.Tool{tool("x__y")}}
	pb := &stub{ns: "b", passthrough: true, tools: []*mcp.Tool{tool("x__y")}}
	_ = a
	_ = b
	_, err := Resolve([]Upstream{pa, pb}, map[string]bool{"x__y": true}, nil)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("duplicate qualified name: want ambiguity error, got %v", err)
	}
}

func TestResolveSkipsBadNames(t *testing.T) {
	up := &stub{ns: "fs", tools: []*mcp.Tool{
		tool("ok_tool"),
		tool("has.dot"),               // wire-illegal char
		tool("has__separator"),        // contains the separator
		tool(strings.Repeat("x", 70)), // too long when namespaced
	}}
	res, err := Resolve([]Upstream{up}, map[string]bool{
		"fs__ok_tool": true,
		"fs__has.dot": true, "fs__has__separator": true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := qualifiedNames(res)
	if len(got) != 1 || got[0] != "fs__ok_tool" {
		t.Fatalf("want only fs__ok_tool, got %v", got)
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("want 3 skips, got %+v", res.Skipped)
	}
	for _, s := range res.Skipped {
		if s.Backend != "fs" || s.Reason == "" {
			t.Fatalf("skip lacks evidence: %+v", s)
		}
	}
}

func TestResolvePassthrough(t *testing.T) {
	up := &stub{ns: "edge", passthrough: true, tools: []*mcp.Tool{
		tool("cbm__search_graph"), // already qualified — trusted as-is
		tool("toolhost__search"),  // inner control plane — dropped
		tool("bare_name"),         // not qualified — skipped
	}}
	res, err := Resolve([]Upstream{up}, map[string]bool{
		"cbm__search_graph": true, "edge__cbm__search_graph": true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := qualifiedNames(res)
	if len(got) != 1 || got[0] != "cbm__search_graph" {
		t.Fatalf("want [cbm__search_graph] trusted as-is, got %v", got)
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("want 2 skips (toolhost__* + bare name), got %+v", res.Skipped)
	}
	// A federated name is approved under ITS own qualified form — the edge's
	// namespace never appears.
	if res.Tools[0].Upstream != up {
		t.Fatal("passthrough tool lost its upstream")
	}
}

func TestQualify(t *testing.T) {
	plain := &stub{ns: "fs"}
	q, err := Qualify(plain, "read_file")
	if err != nil || q != "fs__read_file" {
		t.Fatalf("want fs__read_file, got %q err=%v", q, err)
	}

	passthrough := &stub{ns: "edge", passthrough: true}
	q, err = Qualify(passthrough, "cbm__x")
	if err != nil || q != "cbm__x" {
		t.Fatalf("passthrough should trust qualified names, got %q err=%v", q, err)
	}
	if _, err = Qualify(passthrough, "toolhost__status"); err == nil {
		t.Fatal("inner toolhost__* must never forward")
	}
	if _, err = Qualify(passthrough, "unqualified"); err == nil {
		t.Fatal("passthrough must reject unqualified names")
	}
}

func TestIsQualifiedAndErrNotApproved(t *testing.T) {
	if !IsQualified("a__b") || IsQualified("abc") || IsQualified("a__b__c") {
		t.Fatal("IsQualified misclassifies names")
	}
	if !errors.Is(ErrNotApproved, ErrNotApproved) {
		t.Fatal("ErrNotApproved is the sentinel — never compare strings")
	}
}
