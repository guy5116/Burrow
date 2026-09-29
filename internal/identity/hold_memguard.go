//go:build memguard

package identity

import (
	"github.com/awnumar/memguard"

	"github.com/guy5116/burrow/internal/secret"
)

// hold moves b into memory that is locked against swapping, excluded from
// core dumps where the OS allows, surrounded by guard pages and read-only.
// b is wiped. A failure to lock (RLIMIT_MEMLOCK too low, for instance) is an
// error, never a silent fall back to ordinary memory.
func hold(b []byte) (storage []byte, release func(), err error) {
	defer func() {
		if r := recover(); r != nil {
			secret.Wipe(b)
			storage, release, err = nil, nil, ErrLock
		}
	}()
	lb := memguard.NewBufferFromBytes(b)
	if lb.Size() != len(lb.Bytes()) || lb.Size() == 0 {
		lb.Destroy()
		return nil, nil, ErrLock
	}
	return lb.Bytes(), lb.Destroy, nil
}

// Locked reports whether identity keys live in locked memory (true in this build).
func Locked() bool { return true }
