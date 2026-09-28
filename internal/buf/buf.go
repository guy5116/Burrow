// Package buf pools frame-sized buffers for the hot path so that no
// frame-sized allocation happens per frame (CLAUDE.md §11). Buffers are wiped
// on Put because they may have held plaintext.
package buf

import "sync"

// Size is one outer frame: 4-byte length prefix + wire.MaxCiphertext.
// wire tests assert the two agree (buf must not import wire).
const Size = 4 + 65552

var pool = sync.Pool{New: func() any { b := make([]byte, Size); return &b }}

// Get returns a buffer of len Size. Contents are zero.
func Get() *[]byte { return pool.Get().(*[]byte) }

// Put wipes b and returns it to the pool. Nil is ignored.
func Put(b *[]byte) {
	if b == nil || cap(*b) < Size {
		return
	}
	*b = (*b)[:Size]
	clear(*b)
	pool.Put(b)
}
