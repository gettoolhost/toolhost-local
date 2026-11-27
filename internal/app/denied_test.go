package app

import (
	"testing"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// A human may deny without a reason. The agent must still see the verdict,
// or it re-asks into a wall it can't see.
func TestCatalogShowsDenialWithoutReason(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	live, cfg, cfgPath := testLive(t)
	cfg.Approved = nil
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	live.ups["a"] = newFakeUp("a", "x")
	if err := config.SaveRequests(cfgPath, []config.Request{{
		Kind: config.RequestTool, Name: "a__x", At: time.Now(), Denied: true,
	}}); err != nil {
		t.Fatal(err)
	}
	live.mu.Lock()
	cat := live.catalog()
	live.mu.Unlock()
	if len(cat) != 1 || cat[0].Qualified != "a__x" {
		t.Fatalf("catalog = %+v, want one entry a__x", cat)
	}
	if !cat[0].Denied {
		t.Error("a__x not marked Denied: a denial with an empty reason is invisible to the agent")
	}
	if cat[0].Requested {
		t.Error("a__x marked Requested: a denied petition is decided, not pending")
	}
}
