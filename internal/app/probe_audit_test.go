package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
)

type recordingSink struct {
	mu  sync.Mutex
	evs []core.Event
}

func (r *recordingSink) Record(e core.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *recordingSink) kinds(tool string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.evs {
		if e.Tool == tool {
			out = append(out, e.Kind)
		}
	}
	return out
}

// An agent that calls a hidden tool by name, without gateway__call, is
// refused by the SDK before any handler runs. The probe must still leave
// one tool_forbidden record, the same as a refusal through gateway__call;
// a listed tool's failure must not be mistaken for a probe.
func TestDirectCallToHiddenToolIsAudited(t *testing.T) {
	live, cfg, _ := testLive(t)
	sink := &recordingSink{}
	live.fd = frontdoor.NewBare(&core.Resolution{}, sink, live.meta())
	up := newFakeUp("a", "x")
	live.ups["a"] = up
	res, err := core.Resolve([]core.Upstream{up}, cfg.ApprovedSet(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.fd.Reload(res)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = live.fd.ServeConn(ctx, serverT) }()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "a__hidden"}); err == nil {
		t.Fatal("precondition: a direct call to an unregistered tool should be refused")
	}
	if got := sink.kinds("a__hidden"); len(got) != 1 || got[0] != core.EventToolForbidden {
		t.Errorf("direct call to a__hidden left records %v, want exactly [%s]", got, core.EventToolForbidden)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "a__x"}); err != nil {
		t.Fatalf("a__x is listed and should dispatch: %v", err)
	}
	if got := sink.kinds("a__x"); len(got) != 1 || got[0] != core.EventToolCall {
		t.Errorf("listed a__x left records %v, want exactly [%s]", got, core.EventToolCall)
	}

	// Through gateway__call the refusal is recorded once, not twice.
	r, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "gateway__call",
		Arguments: map[string]any{"name": "a__hidden2"}})
	if err != nil || r == nil || !r.IsError {
		t.Fatalf("gateway__call a__hidden2 should be refused, err=%v res=%+v", err, r)
	}
	if got := sink.kinds("a__hidden2"); len(got) != 1 || got[0] != core.EventToolForbidden {
		t.Errorf("gateway__call a__hidden2 left records %v, want exactly [%s]", got, core.EventToolForbidden)
	}

	// A served tool whose upstream fails is a failed call, not a probe.
	live.ups["b"] = &failingUp{fakeUp: newFakeUp("b", "y")}
	res, err = core.Resolve([]core.Upstream{up, live.ups["b"]}, map[string]bool{"a__x": true, "b__y": true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	live.fd.Reload(res)
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "b__y"}); err == nil {
		t.Fatal("precondition: b__y's upstream fails, so the call should error")
	}
	if got := sink.kinds("b__y"); len(got) != 1 || got[0] != core.EventToolCall {
		t.Errorf("failed call to served b__y left records %v, want exactly [%s]", got, core.EventToolCall)
	}
}

// failingUp is a served upstream whose every call fails.
type failingUp struct{ *fakeUp }

func (f *failingUp) Call(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return nil, errors.New("upstream down")
}

// A probe's tool name is the agent's choice and can be any length. The
// audit record keeps a bounded prefix, so one call can't bloat the log or
// break `toolhost audit`, which reads it line by line.
func TestProbeNameIsBoundedInTheAudit(t *testing.T) {
	live, _, _ := testLive(t)
	sink := &recordingSink{}
	live.fd = frontdoor.NewBare(&core.Resolution{}, sink, live.meta())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = live.fd.ServeConn(ctx, serverT) }()
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	long := "a__" + strings.Repeat("x", 1<<20)
	_, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: long})
	_, _ = sess.CallTool(ctx, &mcp.CallToolParams{Name: "gateway__call", Arguments: map[string]any{"name": long}})
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.evs) != 2 {
		t.Fatalf("got %d records, want 2 (direct probe, gateway__call refusal)", len(sink.evs))
	}
	for _, e := range sink.evs {
		if len(e.Tool) > 256 {
			t.Errorf("%s record holds a %d-byte tool name, want at most 256", e.Kind, len(e.Tool))
		}
		if !strings.HasPrefix(e.Tool, "a__xxx") {
			t.Errorf("%s record lost the name's prefix: %.20q", e.Kind, e.Tool)
		}
	}
}
