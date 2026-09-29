package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// ErrRelPath is returned for a blob path outside the allowed shape.
var ErrRelPath = errors.New("store: invalid blob path")

// tempPrefix starts the name of a file that writeFileAtomic is still writing.
const tempPrefix = ".tmp-"

// validRelPath allows "name" or "dir/name" with segments of [A-Za-z0-9._-]
// that do not start with a dot. The store's own files (lock, master.hdr,
// master.key) are no blob names, in any letter case: on the default file
// systems of macOS and Windows "LOCK" is the same file as "lock".
func validRelPath(p string) bool {
	segs := strings.Split(p, "/")
	if len(p) > 128 || len(segs) > 2 {
		return false
	}
	for _, seg := range segs {
		lower := strings.ToLower(seg)
		if seg == "" || seg[0] == '.' || lower == lockFile || strings.HasPrefix(lower, "master.") {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

// writeFileAtomic writes data to path via a temp file in the same directory,
// fsync, rename, then fsyncs the directory (Unix).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, tempPrefix+hex.EncodeToString(r[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a rename is durable. No-op on Windows.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncTree fsyncs every file and directory under root.
func syncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(p)
		}
		f, err := os.Open(p) // #nosec G122 -- our own 0700 store directory
		if err != nil {
			return err
		}
		err = f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return err
	})
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
