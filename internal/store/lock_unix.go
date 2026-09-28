//go:build !windows

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// acquireLock takes an exclusive, non-blocking flock on path.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, filePerm)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, ErrInUse
	}
	return f, nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return f.Close()
}
