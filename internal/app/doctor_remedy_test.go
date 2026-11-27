package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// doctor's remedy for a missing grant is a command that exists.
func TestDoctorMissingGrantNamesTheRealCommand(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	path := t.TempDir() + "/toolhost.json"
	f := &config.File{Token: "th_fixture", Listen: "127.0.0.1:1", Backends: map[string]*config.Backend{
		"x": {Transport: "http", URL: "http://127.0.0.1:1/mcp", Auth: &config.Auth{Type: "oauth"}},
	}}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_ = Doctor(context.Background(), path, &out)
	if !strings.Contains(out.String(), "run toolhost mcp auth x") {
		t.Errorf("doctor's remedy names no real command; got:\n%s", out.String())
	}
}
