package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/audit"
	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
	"github.com/gettoolhost/toolhost-local/internal/upstream"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeUp is a dialable upstream whose session death is observable: Wait
// unblocks when the session drops (or Close is called). close() simulates
// a mid-flight session loss without going through Close's bookkeeping.
type fakeUp struct {
	ns     string
	tools  []*mcp.Tool
	dead   chan struct{}
	closed bool
}

func newFakeUp(ns string, tools ...string) *fakeUp {
	var ts []*mcp.Tool
	for _, n := range tools {
		ts = append(ts, &mcp.Tool{Name: n, InputSchema: map[string]any{"type": "object"}})
	}
	return &fakeUp{ns: ns, tools: ts, dead: make(chan struct{})}
}

func (f *fakeUp) Namespace() string  { return f.ns }
func (f *fakeUp) Tools() []*mcp.Tool { return f.tools }
func (f *fakeUp) Call(context.Context, string, json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{}, nil
}
func (f *fakeUp) Close() error { f.closed = true; f.kill(); return nil }
func (f *fakeUp) Wait() error  { <-f.dead; return errors.New("session dropped") }
func (f *fakeUp) kill() {
	select {
	case <-f.dead:
	default:
		close(f.dead)
	}
}

// testLive builds a liveSet against a real on-disk config + audit sink and
// a real frontdoor, with an injectable dial.
func testLive(t *testing.T) (*liveSet, *config.File, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "toolhost.json")
	cfg := &config.File{
		Backends: map[string]*config.Backend{
			"a": {Transport: "http", URL: "http://a.invalid"},
		},
		Approved: []string{"a__x"},
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	sink, err := audit.Open(filepath.Join(dir, "audit.jsonl"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	live := &liveSet{
		path:        cfgPath,
		fd:          frontdoor.NewBare(&core.Resolution{}, nil, nil),
		sink:        sink,
		w:           io.Discard,
		ctx:         context.Background(),
		ups:         map[string]core.Upstream{},
		cfgs:        map[string]*config.Backend{"a": cfg.Backends["a"]},
		opts:        &upstream.Options{},
		dead:        make(chan deadSig, 8),
		deadPending: map[string]deadSig{},
		retry:       map[string]*retryState{},
	}
	return live, cfg, cfgPath
}

// Session death drops the backend, the backoff sweep redials it, and the
// fresh session takes the slot.
func TestReconnectReplacesDeadBackend(t *testing.T) {
	live, _, _ := testLive(t)
	up1 := newFakeUp("a", "x")
	live.ups["a"] = up1

	// The session dies mid-flight — same signal the watcher goroutine sends.
	up1.kill()
	live.dead <- deadSig{name: "a", up: up1, err: errors.New("dropped")}

	live.mu.Lock()
	changed := live.drainDead()
	live.retry["a"].nextAt = time.Now().Add(-time.Second) // backoff elapsed
	live.mu.Unlock()
	if !changed {
		t.Fatal("dead signal should have dropped the backend")
	}

	up2 := newFakeUp("a", "x")
	var dialed int
	live.dial = func(_ context.Context, name string, _ *config.Backend, _ *upstream.Options) (core.Upstream, error) {
		dialed++
		return up2, nil
	}
	live.retrySweep()

	live.mu.Lock()
	defer live.mu.Unlock()
	if live.ups["a"] != up2 {
		t.Fatalf("fresh session should hold the slot, got %+v", live.ups)
	}
	if _, pending := live.retry["a"]; pending {
		t.Fatal("a successful reconnect must clear the backoff")
	}
	if dialed != 1 {
		t.Fatalf("want exactly one dial, got %d", dialed)
	}
}

// A death signal carries the session identity — a stale signal for an
// already-replaced session must not ghost-kill its successor.
func TestStaleDeathSignalIgnored(t *testing.T) {
	live, _, _ := testLive(t)
	old, cur := newFakeUp("a", "x"), newFakeUp("a", "x")
	live.ups["a"] = cur
	live.dead <- deadSig{name: "a", up: old, err: errors.New("old session ended")}

	live.mu.Lock()
	changed := live.drainDead()
	live.mu.Unlock()
	if changed || live.ups["a"] != cur {
		t.Fatal("stale signal must not disturb the live session")
	}
}

// A config reload racing a redial wins: the fresher party owns the slot and
// the in-flight session closes unclaimed.
func TestConfigChangeMidDialDiscardsResult(t *testing.T) {
	live, _, _ := testLive(t)
	live.retry["a"] = &retryState{attempts: 1, nextAt: time.Now().Add(-time.Second)}

	stale := newFakeUp("a", "x")
	live.dial = func(_ context.Context, name string, _ *config.Backend, _ *upstream.Options) (core.Upstream, error) {
		// The human edits the config while we're dialing.
		live.mu.Lock()
		live.cfgs["a"] = &config.Backend{Transport: "http", URL: "http://b.invalid"}
		live.mu.Unlock()
		return stale, nil
	}
	live.retrySweep()

	live.mu.Lock()
	defer live.mu.Unlock()
	if _, ok := live.ups["a"]; ok {
		t.Fatal("stale dial result must not take the slot")
	}
	if !stale.closed {
		t.Fatal("the discarded session must be closed, not leaked")
	}
	// The retry entry survives — the sweep will dial the NEW config next.
	if _, ok := live.retry["a"]; !ok {
		t.Fatal("discarded dial should leave the backend retryable")
	}
}

// A failed redial keeps the backend down and pushes the next attempt
// further out — never a tight loop.
func TestFailedRedialBacksOff(t *testing.T) {
	live, _, _ := testLive(t)
	live.retry["a"] = &retryState{attempts: 1, nextAt: time.Now().Add(-time.Second)}
	live.dial = func(_ context.Context, _ string, _ *config.Backend, _ *upstream.Options) (core.Upstream, error) {
		return nil, errors.New("connection refused")
	}
	live.retrySweep()

	live.mu.Lock()
	defer live.mu.Unlock()
	if _, ok := live.ups["a"]; ok {
		t.Fatal("failed dial must not appear live")
	}
	st := live.retry["a"]
	if st == nil || st.attempts != 2 || st.nextAt.Before(time.Now()) {
		t.Fatalf("failed dial should reschedule, got %+v", st)
	}
}

// Backoff doubles per attempt and never exceeds the cap (+jitter ≤ 25%).
// Jitter means delay isn't strictly monotonic near the cap — that's the
// point — so assert the floor and ceiling instead of ordering.
func TestRetryBackoffBounds(t *testing.T) {
	live, _, _ := testLive(t)
	live.mu.Lock()
	defer live.mu.Unlock()
	for i := 1; i <= 20; i++ {
		live.markRetry("a")
		delay := time.Until(live.retry["a"].nextAt)
		base := retryBase << min(i-1, 8)
		if base > retryCap {
			base = retryCap
		}
		if delay < base {
			t.Fatalf("attempt %d: delay %v under the %v floor", i, delay, base)
		}
		if delay > retryCap+retryCap/4 {
			t.Fatalf("attempt %d: delay %v exceeds cap+jitter", i, delay)
		}
	}
}

// Removing a backend from config sweeps its retry entry — a dead-and-gone
// backend never redials.
func TestRemovedBackendStopsRetrying(t *testing.T) {
	live, cfg, cfgPath := testLive(t)
	live.mu.Lock()
	live.markRetry("a")
	live.mu.Unlock()

	// Human removes the backend; the next reload purges every trace —
	// including cfgs, which is what a mid-flight dial compares against.
	delete(cfg.Backends, "a")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	live.reload()

	live.mu.Lock()
	defer live.mu.Unlock()
	if _, ok := live.cfgs["a"]; ok {
		t.Fatal("removed backend left a stale cfg")
	}
	if len(live.retry) != 0 {
		t.Fatalf("removed backend still scheduled for retry: %v", live.retry)
	}
}

// The resurrection path: a retry dial in flight while the backend is
// removed must close unclaimed — reload purges l.cfgs, so the commit
// check sees nil vs the dialed cfg and refuses.
func TestReloadDuringRetryKillsInFlightDial(t *testing.T) {
	live, cfg, cfgPath := testLive(t)
	live.retry["a"] = &retryState{attempts: 1, nextAt: time.Now().Add(-time.Second)}

	stale := newFakeUp("a", "x")
	live.dial = func(_ context.Context, name string, _ *config.Backend, _ *upstream.Options) (core.Upstream, error) {
		// The human removes the backend mid-dial; the reload purges
		// cfgs+retry before our result commits.
		delete(cfg.Backends, "a")
		if err := cfg.Save(cfgPath); err != nil {
			t.Fatal(err)
		}
		live.reload()
		return stale, nil
	}
	live.retrySweep()

	live.mu.Lock()
	defer live.mu.Unlock()
	if _, ok := live.ups["a"]; ok {
		t.Fatal("removed backend resurrected via in-flight dial")
	}
	if !stale.closed {
		t.Fatal("discarded session must be closed")
	}
}

// A transport-level Call failure is a death signal — HTTP sessions can
// look alive after the wire dies. Caller-side timeouts and stale sessions
// are NOT death.
func TestCallErrorDeathSignal(t *testing.T) {
	live, _, _ := testLive(t)
	up := newFakeUp("a", "x")
	live.ups["a"] = up

	live.reportCallError(up, context.DeadlineExceeded)
	select {
	case <-live.dead:
		t.Fatal("caller-side timeout must not kill the backend")
	default:
	}

	live.reportCallError(up, errors.New("connection reset by peer"))
	select {
	case sig := <-live.dead:
		if sig.up != up || sig.name != "a" {
			t.Fatalf("bad signal: %+v", sig)
		}
	default:
		t.Fatal("transport error should queue a death signal")
	}

	// A stale session's error kills nothing — the slot belongs to another.
	live.reportCallError(newFakeUp("a", "x"), errors.New("reset"))
	select {
	case sig := <-live.dead:
		t.Fatalf("stale session's error signaled death: %+v", sig)
	default:
	}
}

// A full dead channel must never stall Call handlers — a flapping session
// emits one signal per failing call while retrySweep sits inside a dial.
// Dropped signals are safe: the backend stays dead and the next failing
// call re-signals.
func TestSignalDeathNeverBlocks(t *testing.T) {
	live, _, _ := testLive(t)
	for i := 0; i < cap(live.dead); i++ {
		live.dead <- deadSig{name: "a"}
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 64; i++ {
			live.signalDeath(deadSig{name: "a", err: errors.New("still down")})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("signalDeath blocked on a full dead channel")
	}
}

// If the live log is deleted but <path>.1 survives, toolhost__audit still
// returns the retained history — the rotated segment isn't lost evidence.
func TestAuditTailSurvivesLiveDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	s, err := audit.Open(path, 300)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		s.Record(core.Event{TS: time.Now(), Kind: core.EventToolCall, Tool: "a__x"})
	}
	s.Close()
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Skip("rotation didn't happen — cap accounting changed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	events, err := auditTail(path, 50, "")
	if err != nil {
		t.Fatalf("auditTail must read .1 when the live file is gone: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("rotated generation returned no events")
	}
}

// After rotation, toolhost__audit must still see across the seam: the live
// segment backfills from <path>.1.
func TestAuditTailReadsRotatedGeneration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	s, err := audit.Open(path, 300) // ~1 record per segment
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 200)
	for _, n := range []string{"one", "two", "three"} {
		s.Record(core.Event{TS: time.Now(), Kind: core.EventToolCall, Tool: n + pad})
	}
	s.Close()

	evs, err := auditTail(path, 10, "")
	if err != nil {
		t.Fatal(err)
	}
	// Live holds "three", .1 holds "two" — tail must span both.
	if len(evs) != 2 {
		t.Fatalf("tail must backfill from the rotated generation, got %d events", len(evs))
	}
	if !strings.HasPrefix(evs[0].Tool, "two") || !strings.HasPrefix(evs[1].Tool, "three") {
		t.Fatalf("wrong order across the rotation seam: %+v", evs)
	}
}

// A death signal that can't queue is parked, not dropped — drainDead
// picks it up on the next sweep and the dead backend still comes out.
func TestSignalDeathParksWhenFull(t *testing.T) {
	live, _, _ := testLive(t)
	up1 := newFakeUp("a", "x")
	live.ups["a"] = up1
	for i := 0; i < cap(live.dead); i++ {
		live.dead <- deadSig{name: "filler"}
	}
	live.signalDeath(deadSig{name: "a", up: up1, err: errors.New("dropped")})
	if live.deadPending["a"].up != up1 {
		t.Fatal("overflow signal was not parked in deadPending")
	}
	live.mu.Lock()
	changed := live.drainDead()
	_, alive := live.ups["a"]
	live.mu.Unlock()
	if !changed || alive {
		t.Fatalf("parked signal was lost — backend still live: changed=%v alive=%v", changed, alive)
	}
	if len(live.deadPending) != 0 {
		t.Fatal("deadPending not cleared after drain")
	}
}
