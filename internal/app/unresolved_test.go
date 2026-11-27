package app

// A resolve that fails serves no server tools until it resolves again. That
// fails closed: the surface already on the wire could hold an approval
// revoked since. It is safe, but to an agent it looks like an outage, so it
// has to be loud too. Status, doctor and GET /status say what failed and
// since when, and doctor fails for as long as it lasts.

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
)

const noToolsLine = "serving no tools from any server"

func resolveError(t *testing.T, live *liveSet) (map[string]any, bool) {
	t.Helper()
	report, ok := live.metaReport().(map[string]any)
	if !ok {
		t.Fatalf("metaReport is %T, want map[string]any", live.metaReport())
	}
	re, ok := report["resolve_error"].(map[string]any)
	return re, ok
}

func publishNow(t *testing.T, live *liveSet, path string) int {
	t.Helper()
	f, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	defer live.mu.Unlock()
	return live.publish(f)
}

func TestResolveFailureIsLoudWhileItLasts(t *testing.T) {
	live, _, cfgPath := testLive(t)
	live.cfgs = map[string]*config.Backend{}
	healthyConfig(t, cfgPath, "127.0.0.1:1")
	live.fd = frontdoor.NewBare(&core.Resolution{}, live.sink, live.meta())
	live.ups["a"] = newFakeUp("a", "x")
	if n := publishNow(t, live, cfgPath); n < 0 {
		t.Fatal("precondition: publish failed")
	}
	if out, err := doctorOut(t, cfgPath); err != nil || strings.Contains(out, noToolsLine) {
		t.Fatalf("precondition: doctor on a healthy gateway, err=%v\n%s", err, out)
	}

	// A server whose inventory lists one name twice: nothing can route it.
	before := time.Now()
	live.ups["bad"] = newFakeUp("bad", "y", "y")
	if n := publishNow(t, live, cfgPath); n != -1 {
		t.Fatalf("publish = %d, want -1", n)
	}

	re, ok := resolveError(t, live)
	if !ok {
		t.Fatal("GET /status report has no resolve_error while the gateway serves no tools")
	}
	if msg, _ := re["error"].(string); !strings.Contains(msg, "claimed by both") {
		t.Errorf("resolve_error.error = %q, want the clash", msg)
	}
	since, _ := re["since"].(time.Time)
	if since.Before(before.Add(-time.Second)) || since.After(time.Now()) {
		t.Errorf("resolve_error.since = %v, want the moment the resolve failed", re["since"])
	}

	out, err := doctorOut(t, cfgPath)
	if err == nil {
		t.Error("doctor passed while the gateway serves no tools")
	}
	for _, want := range []string{"could not resolve the tools since", "claimed by both", noToolsLine} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}

	// The gateway is on a port nothing answers, as a stdio gateway would
	// be: status still says so.
	var st bytes.Buffer
	_ = Status(context.Background(), cfgPath, false, &st)
	if !strings.Contains(st.String(), noToolsLine) {
		t.Errorf("status output lacks %q:\n%s", noToolsLine, st.String())
	}

	// Failing again keeps the first failure's time.
	if n := publishNow(t, live, cfgPath); n != -1 {
		t.Fatalf("publish = %d, want -1", n)
	}
	if re, _ := resolveError(t, live); !re["since"].(time.Time).Equal(since) {
		t.Errorf("since moved from %v to %v on a second failed resolve", since, re["since"])
	}

	// The clash goes; the tools come back and the warning goes with them.
	delete(live.ups, "bad")
	if n := publishNow(t, live, cfgPath); n < 0 {
		t.Fatal("publish still failing without the clash")
	}
	if _, ok := resolveError(t, live); ok {
		t.Error("resolve_error still reported after a good resolve")
	}
	if out, err := doctorOut(t, cfgPath); err != nil || strings.Contains(out, noToolsLine) {
		t.Errorf("doctor after the clash went, err=%v\n%s", err, out)
	}
}

func TestStatusShowsTheGatewaysResolveError(t *testing.T) {
	live, _, cfgPath := testLive(t)
	live.cfgs = map[string]*config.Backend{}
	fd, err := frontdoor.New(&core.Resolution{}, "fixture-invalid-token", live.sink, live.meta(), &frontdoor.Options{Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	live.fd = fd
	srv := httptest.NewServer(fd.Handler())
	t.Cleanup(srv.Close)
	live.listen = strings.TrimPrefix(srv.URL, "http://")
	healthyConfig(t, cfgPath, live.listen)

	live.ups["bad"] = newFakeUp("bad", "y", "y")
	if n := publishNow(t, live, cfgPath); n != -1 {
		t.Fatalf("publish = %d, want -1", n)
	}

	var st bytes.Buffer
	if err := Status(context.Background(), cfgPath, false, &st); err != nil {
		t.Fatalf("status: %v\n%s", err, st.String())
	}
	if !strings.Contains(st.String(), "gateway: UP") || strings.Count(st.String(), noToolsLine) != 1 {
		t.Errorf("status should carry the resolve error once:\n%s", st.String())
	}
	var js bytes.Buffer
	if err := Status(context.Background(), cfgPath, true, &js); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"resolve_error"`) {
		t.Errorf("status --json lacks resolve_error:\n%s", js.String())
	}
}

// Both at once: the tools stopped resolving, then a broken edit stopped the
// config loading. Each fault is reported, and each ends on its own.
func TestBothFaultsAreReportedAndEndApart(t *testing.T) {
	live, _, cfgPath := testLive(t)
	// The clashing server is in the config, so a reload keeps its session
	// rather than closing it. Its command is on PATH, so doctor's own
	// checks pass.
	withBad := func() {
		t.Helper()
		f := &config.File{Listen: "127.0.0.1:1", Token: "fixture-invalid-token", AuditLog: "audit.jsonl",
			Backends: map[string]*config.Backend{"bad": {Transport: "stdio", Command: "true"}}}
		if err := f.Save(cfgPath); err != nil {
			t.Fatal(err)
		}
	}
	withBad()
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	live.cfgs = map[string]*config.Backend{"bad": loaded.Backends["bad"]}
	live.fd = frontdoor.NewBare(&core.Resolution{}, live.sink, live.meta())
	live.ups["bad"] = newFakeUp("bad", "y", "y")
	live.reload()
	if _, ok := resolveError(t, live); !ok {
		t.Fatal("precondition: the clash fails the resolve")
	}
	breakConfig(t, cfgPath)
	live.reload()

	out, err := doctorOut(t, cfgPath)
	if err == nil || !strings.Contains(out, previousLine) || !strings.Contains(out, noToolsLine) {
		t.Errorf("doctor with both faults, err=%v\n%s", err, out)
	}
	var st bytes.Buffer
	_ = Status(context.Background(), cfgPath, false, &st)
	if !strings.Contains(st.String(), previousLine) || !strings.Contains(st.String(), noToolsLine) {
		t.Errorf("status with both faults:\n%s", st.String())
	}

	// The config loads again, but the clash is still there.
	withBad()
	live.reload()
	out, err = doctorOut(t, cfgPath)
	if err == nil || strings.Contains(out, previousLine) || !strings.Contains(out, noToolsLine) {
		t.Errorf("doctor with only the clash left, err=%v\n%s", err, out)
	}

	// The server stops listing the name twice.
	live.ups["bad"] = newFakeUp("bad", "y")
	live.reload()
	if out, err := doctorOut(t, cfgPath); err != nil || strings.Contains(out, noToolsLine) || strings.Contains(out, previousLine) {
		t.Errorf("doctor with both faults gone, err=%v\n%s", err, out)
	}
}

// The gateway that answered /status has said its piece; every other
// gateway on the config, such as a stdio one, still reports.
func TestStatusPrintsOtherGatewaysOnce(t *testing.T) {
	fault := &config.Fault{Since: time.Now(), Error: "boom"}
	var out bytes.Buffer
	printDegraded(&out, []config.Degraded{
		{PID: 1, Listen: "127.0.0.1:7", ResolveError: fault},
		{PID: 2, ResolveError: fault},
	}, "127.0.0.1:7")
	if strings.Contains(out.String(), "pid 1") || !strings.Contains(out.String(), "pid 2 over stdio") {
		t.Errorf("printDegraded:\n%s", out.String())
	}
}
