package app

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
)

// A resolve that fails must never leave the previous surface standing: the
// previous surface can hold approvals revoked since. Here an approval is
// revoked in the same publish that meets an unrelated name collision — the
// revoked tool has to stop answering anyway.
func TestPublishFailsClosedOnResolveError(t *testing.T) {
	live, cfg, cfgPath := testLive(t)
	live.fd = frontdoor.NewBare(&core.Resolution{}, live.sink, live.meta())
	up := newFakeUp("a", "x")
	live.ups["a"] = up

	res, err := core.Resolve([]core.Upstream{up}, cfg.ApprovedSet(), nil, nil)
	if err != nil {
		t.Fatalf("initial Resolve: %v", err)
	}
	live.fd.Reload(res)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = live.fd.ServeConn(ctx, serverT) }()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer sess.Close()

	if r, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "a__x"}); err != nil || r.IsError {
		t.Fatalf("precondition: a__x callable before revocation, err=%v res=%+v", err, r)
	}

	// An unrelated backend whose inventory lists one name twice: Resolve
	// can't route it, so it errors.
	live.ups["bad"] = newFakeUp("bad", "y", "y")
	cfg.Approved = nil
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("save revoked config: %v", err)
	}
	live.mu.Lock()
	n := live.publish(cfg)
	live.mu.Unlock()
	if n != -1 {
		t.Fatalf("publish = %d, want -1 (resolve failed)", n)
	}

	r, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "a__x"})
	if err == nil && r != nil && !r.IsError {
		t.Fatal("a__x still callable after its approval was revoked — a failed resolve kept the old surface")
	}

	// The control plane survives: an agent can still discover and ask.
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == "a__x" {
			t.Fatal("a__x still listed after a failed resolve")
		}
	}
	if len(tools.Tools) != 5 {
		t.Fatalf("want only the 5 gateway__ verbs listed, got %d tools", len(tools.Tools))
	}
	r, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "gateway__call",
		Arguments: map[string]any{"name": "a__x"}})
	if err != nil || r == nil || !r.IsError {
		t.Fatalf("gateway__call a__x must be refused after a failed resolve, err=%v res=%+v", err, r)
	}
}
