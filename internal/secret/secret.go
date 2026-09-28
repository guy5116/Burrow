// Package secret holds byte buffers that are wiped when no longer needed.
// Every secret Burrow owns (scalars, seeds, chain keys, the store master key)
// lives in a Buffer. Zeroization under Go's GC is best-effort: the runtime may
// have copied the bytes (stack growth, library internals); see docs/SECURITY.md.
package secret

import "crypto/rand"

// Buffer is a fixed-size secret. Clear it as soon as it is no longer needed.
type Buffer struct{ b []byte }

// New allocates a zeroed Buffer of n bytes.
func New(n int) *Buffer { return &Buffer{b: make([]byte, n)} }

// Random allocates a Buffer filled from crypto/rand.
func Random(n int) (*Buffer, error) {
	s := New(n)
	if _, err := rand.Read(s.b); err != nil {
		return nil, err
	}
	return s, nil
}

// From copies b into a new Buffer and wipes b.
func From(b []byte) *Buffer {
	s := New(len(b))
	copy(s.b, b)
	Wipe(b)
	return s
}

// Bytes returns the underlying slice. Do not retain it past Clear.
func (s *Buffer) Bytes() []byte {
	if s == nil {
		return nil
	}
	return s.b
}

// Len returns the size in bytes.
func (s *Buffer) Len() int {
	if s == nil {
		return 0
	}
	return len(s.b)
}

// Clear overwrites the contents with zeros. Safe to call more than once or on nil.
func (s *Buffer) Clear() {
	if s != nil {
		Wipe(s.b)
	}
}

// Wipe zeros b in place.
func Wipe(b []byte) { clear(b) }
