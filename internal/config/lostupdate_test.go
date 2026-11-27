package config

// Lost updates. The CLI, the running gateway and the human's editor all
// write the same two files. A writer holding an old snapshot must never
// overwrite a newer file, and writers that run at once must all land or
// be told they didn't.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The human revokes a__x; an agent whose snapshot predates the revoke
// enables a__x and saves. The stale save must be refused, not written.
func TestStaleConfigSaveCannotResurrectRevoke(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "toolhost.json")
	empty := []string{}
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{"a__x"}, Enabled: &empty}
	if err := seed.Save(path); err != nil {
		t.Fatalf("seed: %v", err)
	}

	human, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if removed := human.Revoke("a__x"); len(removed) != 1 {
		t.Fatalf("revoke removed %v, want [a__x]", removed)
	}
	if err := human.Save(path); err != nil {
		t.Fatalf("human save: %v", err)
	}

	// On the agent's old snapshot a__x still looks approved.
	if _, err := agent.Enable("a__x"); err != nil {
		t.Fatalf("enable on the old snapshot: %v", err)
	}
	if err := agent.Save(path); err == nil {
		t.Errorf("a save from an old snapshot overwrote a newer config")
	}

	final, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.IsApproved("a__x") {
		t.Errorf("revoked a__x is approved again")
	}
}

// Writers race on one config. Each save either lands or returns an error:
// a save that reports success must never be erased by another.
func TestConcurrentConfigSavesNeverDropAReportedSuccess(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "toolhost.json")
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{}}
	if err := seed.Save(path); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 16
	landed := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			f, err := Load(path)
			if err != nil {
				return // a torn read is a failure the caller sees
			}
			if _, err := f.Approve(map[string]string{fmt.Sprintf("b__t%02d", i): ""}); err != nil {
				return
			}
			landed[i] = f.Save(path) == nil
		}(i)
	}
	close(start)
	wg.Wait()

	final, err := Load(path)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	for i, ok := range landed {
		if name := fmt.Sprintf("b__t%02d", i); ok && !final.IsApproved(name) {
			t.Errorf("save of %s reported success, but the approval is gone", name)
		}
	}
}

// An agent files petitions while the human denies one. Every petition
// and the denial must all be in the queue afterwards.
func TestConcurrentQueueWritersLoseNothing(t *testing.T) {
	t.Setenv("TOOLHOST_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "toolhost.json")
	if _, err := AppendRequests(path, nil, "", "a__x"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 16
	errs := make(chan error, n+1)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := AppendRequests(path, nil, "", fmt.Sprintf("b__t%02d", i))
			errs <- err
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		errs <- DenyRequest(path, "a__x", "not this one")
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("writer failed: %v", err)
		}
	}

	final, err := LoadRequests(path)
	if err != nil {
		t.Fatalf("final load: %v", err)
	}
	if why, ok := DeniedSet(final)["a__x"]; !ok || why != "not this one" {
		t.Errorf("the denial of a__x was lost")
	}
	pending := RequestSet(final)
	for i := 0; i < n; i++ {
		if name := fmt.Sprintf("b__t%02d", i); !pending[name] {
			t.Errorf("petition %s was lost", name)
		}
	}
}

// A stale save names the conflict and leaves the newer file byte for byte.
func TestStaleSaveReturnsErrStaleAndWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolhost.json")
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{"a__x"}}
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}
	old, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(path, func(f *File) error { f.Revoke("a__x"); return nil }); err != nil {
		t.Fatalf("update: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old.Approve(map[string]string{"b__y": ""})
	if err := old.Save(path); !errors.Is(err, ErrStale) {
		t.Fatalf("stale save: got %v, want ErrStale", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("a refused save changed the file")
	}
	// The copy Update returned, and one saved after a fresh Load, save fine.
	fresh, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh.Approve(map[string]string{"b__y": ""})
	if err := fresh.Save(path); err != nil {
		t.Fatalf("fresh save: %v", err)
	}
	fresh.Approve(map[string]string{"c__z": ""})
	if err := fresh.Save(path); err != nil {
		t.Fatalf("second save of the same copy: %v", err)
	}
}

// Writers that go through Update all land, with no conflicts to retry.
func TestConcurrentUpdatesAllLand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolhost.json")
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{}}
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}
	const n = 16
	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := Update(path, func(f *File) error {
				_, err := f.Approve(map[string]string{fmt.Sprintf("b__t%02d", i): ""})
				return err
			})
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("update failed: %v", err)
		}
	}
	final, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Approved) != n {
		t.Errorf("approved %d tools, want %d: %v", len(final.Approved), n, final.Approved)
	}
}

// An Update that changes nothing leaves the file alone, so the gateway's
// watcher sees no reload.
func TestUpdateWithoutChangeWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "toolhost.json")
	seed := &File{Backends: map[string]*Backend{}, Approved: []string{"a__x"}}
	if err := seed.Save(path); err != nil {
		t.Fatal(err)
	}
	// The first load fills in defaults; from then on the bytes are stable.
	if _, err := Update(path, func(*File) error { return nil }); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(path, func(f *File) error { f.Revoke("not__there"); return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(past) {
		t.Errorf("a no-op update rewrote the file (mtime %v, want %v)", st.ModTime(), past)
	}
}
