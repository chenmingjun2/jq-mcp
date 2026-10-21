//go:build windows

package app

import "os"

// Windows has no flock(2). The health probe in AcquireInstance is still the
// authoritative check; this lock is only a fast-path guard, hence best effort.
// A stale lock file is harmless because it is never consulted on its own.
func lockFile(f *os.File) error   { return nil }
func unlockFile(f *os.File) error { return nil }
