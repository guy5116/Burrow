// Package store keeps identity, contacts, invites and transfer metadata
// encrypted at rest (CLAUDE.md §7): an Argon2id- or random-master-key header,
// one XChaCha20-Poly1305 blob per file bound to its relative path, atomic
// writes, an exclusive process lock, and an atomic directory swap for
// passphrase changes with startup recovery.
package store

import (
	"encoding/binary"
	"errors"
)

// Master header layout: magic "BRWM" || version u8 || mode u8 || t u32 || m u32 (KiB) || p u8 || salt[16].
const (
	headerMagic   = "BRWM"
	headerVersion = 1
	HeaderLen     = 4 + 1 + 1 + 4 + 4 + 1 + 16
	SaltLen       = 16
	MasterKeyLen  = 32

	// ModePassphrase derives the master key with Argon2id.
	ModePassphrase uint8 = 1
	// ModeNone stores a random master key in master.key (--insecure-no-passphrase).
	ModeNone uint8 = 2

	// Argon2id bounds accepted when parsing (§7).
	MinTime   = 1
	MaxTime   = 16
	MinMemKiB = 8 << 10
	MaxMemKiB = 1 << 20
	MinPar    = 1
	MaxPar    = 16
)

// Argon2 holds the Argon2id cost parameters.
type Argon2 struct {
	Time    uint32
	MemKiB  uint32
	Threads uint8
}

// DefaultArgon2 is t=3, m=64 MiB, p=4 (§3.1).
var DefaultArgon2 = Argon2{Time: 3, MemKiB: 64 << 10, Threads: 4}

// Header is the parsed master.hdr.
type Header struct {
	Mode uint8
	KDF  Argon2
	Salt [SaltLen]byte
}

// ErrHeader is returned for a malformed or out-of-bounds header.
var ErrHeader = errors.New("store: malformed master header")

func (a Argon2) valid() bool {
	return a.Time >= MinTime && a.Time <= MaxTime && a.MemKiB >= MinMemKiB && a.MemKiB <= MaxMemKiB &&
		a.Threads >= MinPar && a.Threads <= MaxPar
}

// EncodeHeader serializes h. It fails on out-of-bounds parameters.
func EncodeHeader(h Header) ([]byte, error) {
	if (h.Mode != ModePassphrase && h.Mode != ModeNone) || !h.KDF.valid() {
		return nil, ErrHeader
	}
	b := make([]byte, 0, HeaderLen)
	b = append(b, headerMagic...)
	b = append(b, headerVersion, h.Mode)
	b = binary.BigEndian.AppendUint32(b, h.KDF.Time)
	b = binary.BigEndian.AppendUint32(b, h.KDF.MemKiB)
	b = append(b, h.KDF.Threads)
	return append(b, h.Salt[:]...), nil
}

// ParseHeader parses and bounds-checks master.hdr; anything off → refuse to unlock.
func ParseHeader(b []byte) (Header, error) {
	if len(b) != HeaderLen || string(b[:4]) != headerMagic || b[4] != headerVersion {
		return Header{}, ErrHeader
	}
	h := Header{Mode: b[5], KDF: Argon2{Time: binary.BigEndian.Uint32(b[6:]), MemKiB: binary.BigEndian.Uint32(b[10:]), Threads: b[14]}}
	copy(h.Salt[:], b[15:])
	if (h.Mode != ModePassphrase && h.Mode != ModeNone) || !h.KDF.valid() {
		return Header{}, ErrHeader
	}
	return h, nil
}
