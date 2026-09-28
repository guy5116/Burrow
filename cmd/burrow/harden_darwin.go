//go:build darwin

package main

import "golang.org/x/sys/unix"

// harden disables core dumps.
func harden() { _ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}) }
