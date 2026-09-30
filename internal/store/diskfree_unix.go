//go:build !windows

package store

import "golang.org/x/sys/unix"

// FreeSpace returns the bytes available to this user on the file system holding dir.
func FreeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // #nosec G115 -- non-negative counts
}
