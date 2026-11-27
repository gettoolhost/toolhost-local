package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lockWait bounds how long a writer waits for another toolhost process to
// finish its write. Writes under the lock take milliseconds; a wait this
// long means a stuck process, and the caller gets told rather than hung.
const lockWait = 10 * time.Second

// lockFile takes an exclusive advisory lock on p, creating it if needed,
// and returns the release. The lock belongs to the open file, so the
// kernel drops it if the process dies: a crash never leaves a stale lock.
// Lock files live in ~/.toolhost/locks and are never removed: removing one
// while another writer waits on it would let two writers in at once.
func lockFile(p string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, err
	}
	fh, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		ok, err := tryLock(fh)
		if err != nil {
			fh.Close()
			return nil, fmt.Errorf("lock %s: %w", p, err)
		}
		if ok {
			return func() {
				_ = unlock(fh)
				fh.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			fh.Close()
			return nil, fmt.Errorf("another toolhost process has held %s for over %s — nothing written", p, lockWait)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// configLock is the lock for one config file.
func configLock(path string) string { return lockFor(path) }

// requestsLock is the lock for the env's request queue.
func requestsLock(cfgPath string) string { return lockFor(RequestsPath(cfgPath)) }

// lockFor names the lock for the file at p. It is keyed by p's canonical
// path, so every spelling of one file shares one lock, and it lives in
// the home, so a project folder holding a shared config never gains one.
func lockFor(p string) string {
	c := canonical(p)
	sum := sha256.Sum256([]byte(c))
	return filepath.Join(ConfigHome(), "locks", filepath.Base(c)+"-"+hex.EncodeToString(sum[:6])+".lock")
}

// canonical resolves p to one spelling: absolute, with symlinks resolved.
// A symlinked folder and its target, or /tmp and /private/tmp on macOS,
// name the same file. A file that doesn't exist yet resolves through its
// folder.
func canonical(p string) string {
	abs := absPath(p)
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}
