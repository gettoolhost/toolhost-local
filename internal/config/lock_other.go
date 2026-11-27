//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly || windows)

package config

import "os"

// No advisory lock on this platform. Save still refuses a stale snapshot;
// only two writers landing in the same instant can race.
func tryLock(*os.File) (bool, error) { return true, nil }

func unlock(*os.File) error { return nil }
