//go:build !windows

package core

// markDownloaded marks a received file as coming from elsewhere; only Windows
// has such a mark.
func markDownloaded(string) {}
