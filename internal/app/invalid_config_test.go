package app

// An invalid or unreadable config keeps the gateway on the last good one:
// dropping every tool on a typo would punish the user. But then a hand-
// edited revocation with a syntax error doesn't take effect, so that state
// has to be loud. `toolhost status` and `toolhost doctor` say the config is
// invalid, since when, and that the gateway still runs the previous one, and
// doctor fails for as long as it lasts.

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
	"github.com/gettoolhost/toolhost-local/internal/frontdoor"
)

const previousLine = "still running the previous config"

// healthyConfig is a config doctor has nothing to say against: a token, a
// free port, no backends.
func healthyConfig(t *testing.T, path, listen string) {
	t.Helper()
	f := &config.File{Listen: listen, Token: "fixture-invalid-token", AuditLog: "audit.jsonl", Backends: map[string]*config.Backend{}}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
}

// breakConfig is the hand edit gone wrong: a revocation cut off mid-word.
func breakConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(`{"approved": [`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func doctorOut(t *testing.T, path string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Doctor(context.Background(), path, &out)
	return out.String(), err
}

func configError(t *testing.T, live *liveSet) (map[string]any, bool) {
	t.Helper()
	report, ok := live.metaReport().(map[string]any)
	if !ok {
		t.Fatalf("metaReport is %T, want map[string]any", live.metaReport())
	}
	ce, ok := report["config_error"].(map[string]any)
	return ce, ok
}

func TestInvalidConfigIsLoudWhileTheGatewayRunsThePreviousOne(t *testing.T) {
	live, _, cfgPath := testLive(t)
	live.cfgs = map[string]*config.Backend{}
	healthyConfig(t, cfgPath, "127.0.0.1:1")
	live.reload()

	if out, err := doctorOut(t, cfgPath); err != nil || strings.Contains(out, previousLine) {
		t.Fatalf("precondition: doctor on a healthy config, err=%v\n%s", err, out)
	}

	before := time.Now()
	breakConfig(t, cfgPath)
	live.reload()

	ce, ok := configError(t, live)
	if !ok {
		t.Fatal("GET /status report has no config_error while the gateway runs the previous config")
	}
	if msg, _ := ce["error"].(string); !strings.Contains(msg, "parse config") {
		t.Errorf("config_error.error = %q, want the parse error", msg)
	}
	since, _ := ce["since"].(time.Time)
	if since.Before(before.Add(-time.Second)) || since.After(time.Now()) {
		t.Errorf("config_error.since = %v, want the moment the reload failed", ce["since"])
	}

	out, err := doctorOut(t, cfgPath)
	if err == nil {
		t.Error("doctor passed while the gateway runs the previous config")
	}
	for _, want := range []string{"config invalid since", "parse config", previousLine} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%s", want, out)
		}
	}

	var st bytes.Buffer
	_ = Status(context.Background(), cfgPath, false, &st)
	for _, want := range []string{"config invalid since", previousLine} {
		if !strings.Contains(st.String(), want) {
			t.Errorf("status output lacks %q:\n%s", want, st.String())
		}
	}

	var js bytes.Buffer
	if err := Status(context.Background(), cfgPath, true, &js); err == nil {
		t.Error("status --json succeeded on a config that doesn't load")
	}
	if !strings.Contains(js.String(), `"config_error"`) || !strings.Contains(js.String(), `"gateways"`) {
		t.Errorf("status --json lacks config_error or gateways:\n%s", js.String())
	}

	// A second bad edit keeps the first failure's time and shows the new error.
	if err := os.WriteFile(cfgPath, []byte(`{"listen": 7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	live.reload()
	ce, _ = configError(t, live)
	if s, _ := ce["since"].(time.Time); !s.Equal(since) {
		t.Errorf("since moved from %v to %v on a second failed reload", since, s)
	}
	if out, _ := doctorOut(t, cfgPath); !strings.Contains(out, previousLine) {
		t.Errorf("doctor quiet after a second failed reload:\n%s", out)
	}

	// The fix takes effect and the warning goes with it.
	healthyConfig(t, cfgPath, "127.0.0.1:1")
	live.reload()
	if _, ok := configError(t, live); ok {
		t.Error("config_error still reported after a good reload")
	}
	if out, err := doctorOut(t, cfgPath); err != nil || strings.Contains(out, previousLine) {
		t.Errorf("doctor after the fix, err=%v\n%s", err, out)
	}
}

// The file on disk can parse while the gateway still runs the previous
// config: it hasn't reloaded yet, or it couldn't read what the shell can.
// Status asks the gateway, so it says so; doctor fails.
func TestStatusAndDoctorReportTheGatewaysView(t *testing.T) {
	live, _, cfgPath := testLive(t)
	live.cfgs = map[string]*config.Backend{}
	fd, err := frontdoor.New(&core.Resolution{}, "fixture-invalid-token", live.sink, live.meta(), &frontdoor.Options{Stateless: true})
	if err != nil {
		t.Fatal(err)
	}
	live.fd = fd
	srv := httptest.NewServer(fd.Handler())
	t.Cleanup(srv.Close)
	listen := strings.TrimPrefix(srv.URL, "http://")

	breakConfig(t, cfgPath)
	live.reload()
	healthyConfig(t, cfgPath, listen) // written, not yet reloaded

	var st bytes.Buffer
	if err := Status(context.Background(), cfgPath, false, &st); err != nil {
		t.Fatalf("status: %v\n%s", err, st.String())
	}
	if !strings.Contains(st.String(), "gateway: UP") || !strings.Contains(st.String(), previousLine) {
		t.Errorf("status doesn't carry the gateway's config error:\n%s", st.String())
	}
	var js bytes.Buffer
	if err := Status(context.Background(), cfgPath, true, &js); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), `"config_error"`) {
		t.Errorf("status --json lacks config_error:\n%s", js.String())
	}
	if out, err := doctorOut(t, cfgPath); err == nil || !strings.Contains(out, previousLine) {
		t.Errorf("doctor, err=%v\n%s", err, out)
	}
}

// Deleting the config is the unreadable case: the gateway keeps the
// previous one, says so, and the warning ends with the gateway.
func TestDeletedConfigIsLoudAndEndsWithTheGateway(t *testing.T) {
	live, _, cfgPath := testLive(t)
	live.cfgs = map[string]*config.Backend{}
	healthyConfig(t, cfgPath, "127.0.0.1:1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); live.watch(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	for {
		if _, ok := configError(t, live); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no config_error after the config was deleted")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, err := doctorOut(t, cfgPath); err == nil || !strings.Contains(out, previousLine) {
		t.Errorf("doctor with the config deleted, err=%v\n%s", err, out)
	}

	cancel()
	<-done
	if out, _ := doctorOut(t, cfgPath); strings.Contains(out, previousLine) {
		t.Errorf("warning outlived the gateway:\n%s", out)
	}
}
