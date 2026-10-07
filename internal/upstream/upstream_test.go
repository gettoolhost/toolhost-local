package upstream

import (
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

func envHas(env []string, key, want string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv == prefix+want
		}
	}
	return false
}

func envKeyPresent(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

func TestStdioEnvLeastPrivilege(t *testing.T) {
	t.Setenv("TH_TEST_SECRET", "hunter2")
	t.Setenv("TH_TEST_EXTRA", "extra")

	// Default: baseline only — a credential in the gateway's environment
	// must not flow into an unconfigured backend.
	env := stdioEnv(&config.Backend{Transport: "stdio"})
	if envHas(env, "TH_TEST_SECRET", "hunter2") {
		t.Fatal("credential leaked: default env must be baseline-only")
	}
	if !envKeyPresent(env, "PATH") {
		t.Fatal("baseline should still carry PATH & friends for runtimes")
	}

	// env_allowlist names the vars that may pass — nothing else.
	env = stdioEnv(&config.Backend{Transport: "stdio", EnvAllowlist: []string{"TH_TEST_SECRET"}})
	if !envHas(env, "TH_TEST_SECRET", "hunter2") {
		t.Fatal("env_allowlist should forward the named variable")
	}
	if envHas(env, "TH_TEST_EXTRA", "extra") {
		t.Fatal("unlisted variable leaked past the allowlist")
	}

	// env entries always reach the backend and win on collision.
	env = stdioEnv(&config.Backend{Transport: "stdio",
		EnvAllowlist: []string{"TH_TEST_SECRET"},
		Env:          map[string]string{"TH_TEST_SECRET": "override"}})
	if !envHas(env, "TH_TEST_SECRET", "override") {
		t.Fatal("configured env must win over inherited values")
	}
}

func TestStdioEnvInherit(t *testing.T) {
	t.Setenv("TH_TEST_SECRET", "hunter2")
	env := stdioEnv(&config.Backend{Transport: "stdio", EnvInherit: true})
	if !envHas(env, "TH_TEST_SECRET", "hunter2") {
		t.Fatal("env_inherit should forward the whole environment")
	}
	env = stdioEnv(&config.Backend{Transport: "stdio", EnvInherit: true,
		Env: map[string]string{"TH_TEST_SECRET": "override"}})
	if !envHas(env, "TH_TEST_SECRET", "override") {
		t.Fatal("configured env must still win under env_inherit")
	}
}
