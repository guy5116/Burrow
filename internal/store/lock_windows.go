//go:build windows

package store

import (
	"os"

	"golang.org/x/sys/windows"
)

// acquireLock opens path with no sharing so a second process fails.
func acquireLock(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil,
		windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, ErrInUse
	}
	return os.NewFile(uintptr(h), path), nil
}

func releaseLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
