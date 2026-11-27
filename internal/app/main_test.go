package app

import (
	"os"
	"testing"
)

// TestMain gives the package a throwaway TOOLHOST_HOME. Saving a config
// takes a lock in its state dir, and a request queue lives there too:
// without this, a test that forgets t.Setenv writes into the real
// ~/.toolhost. A test that sets its own home still wins.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "toolhost-test-home-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TOOLHOST_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}
