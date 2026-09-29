//go:build linux

package store

import "golang.org/x/sys/unix"

// HardenProcess disables core dumps and ptrace-based dumping (CLAUDE.md §3.6).
func HardenProcess() {
	_ = unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0})
	_ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)
}
