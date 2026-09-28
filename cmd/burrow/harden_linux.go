//go:build linux

package main

import "golang.org/x/sys/unix"

// harden disables core dumps and ptrace-based dumping (CLAUDE.md §3.6).
func harden() {
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
