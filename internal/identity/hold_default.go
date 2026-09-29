//go:build !memguard

package identity

import "github.com/guy5116/burrow/internal/secret"

// hold copies b into a buffer that is wiped on release, and wipes b.
func hold(b []byte) (storage []byte, release func(), err error) {
	s := secret.From(b)
	return s.Bytes(), s.Clear, nil
}

// Locked reports whether identity keys live in locked memory (false in this build).
func Locked() bool { return false }
