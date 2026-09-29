//go:build darwin

package store

import "golang.org/x/sys/unix"

// HardenProcess disables core dumps.
func HardenProcess() { _ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) }
