// Package identity holds long-term X25519 keys, fingerprints and safety
// numbers (CLAUDE.md §3.3). PeerID is declared here so handshake, store and
// core share one type.
package identity

import (
	"encoding/base32"
	"encoding/binary"
	"errors"
	"strings"

	"golang.org/x/crypto/blake2b"
	"golang.org/x/crypto/curve25519"

	"github.com/guy5116/burrow/internal/secret"
)

// PeerID is a peer's 32-byte X25519 public key. The key is the identity.
type PeerID [32]byte

// Fingerprint length in characters (32 bytes in unpadded base32).
const FingerprintLen = 52

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// ErrFingerprint is returned for a malformed fingerprint.
var ErrFingerprint = errors.New("identity: malformed fingerprint")

// Fingerprint returns the 52-character lowercase base32 form of the key.
func (p PeerID) Fingerprint() string { return strings.ToLower(b32.EncodeToString(p[:])) }

// Short returns the first 8 fingerprint characters (default nickname suffix).
func (p PeerID) Short() string { return p.Fingerprint()[:8] }

// Display returns the fingerprint in groups of four separated by spaces.
func (p PeerID) Display() string {
	fp := p.Fingerprint()
	var sb strings.Builder
	sb.Grow(FingerprintLen + FingerprintLen/4)
	for i := 0; i < FingerprintLen; i += 4 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(fp[i : i+4])
	}
	return sb.String()
}

// ParseFingerprint accepts the 52-character form, with or without spaces or
// dashes between groups, in either case.
func ParseFingerprint(s string) (PeerID, error) {
	clean := strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' {
			return -1
		}
		return r
	}, s)
	if len(clean) != FingerprintLen {
		return PeerID{}, ErrFingerprint
	}
	b, err := b32.DecodeString(strings.ToUpper(clean))
	if err != nil || len(b) != 32 {
		return PeerID{}, ErrFingerprint
	}
	var p PeerID
	copy(p[:], b)
	if p.Fingerprint() != strings.ToLower(clean) { // reject non-canonical trailing bits
		return PeerID{}, ErrFingerprint
	}
	return p, nil
}

// Identity is a long-term keypair. The scalar lives in a secret buffer.
type Identity struct {
	scalar *secret.Buffer
	pub    PeerID
}

// Generate creates a fresh identity from crypto/rand.
func Generate() (*Identity, error) {
	s, err := secret.Random(curve25519.ScalarSize)
	if err != nil {
		return nil, err
	}
	return fromBuffer(s)
}

// FromScalar builds an identity from a stored 32-byte scalar. The input is
// wiped.
func FromScalar(scalar []byte) (*Identity, error) {
	if len(scalar) != curve25519.ScalarSize {
		secret.Wipe(scalar)
		return nil, errors.New("identity: scalar must be 32 bytes")
	}
	return fromBuffer(secret.From(scalar))
}

func fromBuffer(s *secret.Buffer) (*Identity, error) {
	pub, err := curve25519.X25519(s.Bytes(), curve25519.Basepoint)
	if err != nil {
		s.Clear()
		return nil, err
	}
	id := &Identity{scalar: s}
	copy(id.pub[:], pub)
	return id, nil
}

// Public returns the public key.
func (id *Identity) Public() PeerID { return id.pub }

// Scalar exposes the private scalar for the handshake. Do not retain or copy
// it; it becomes zero after Clear.
func (id *Identity) Scalar() []byte { return id.scalar.Bytes() }

// Clear wipes the private scalar.
func (id *Identity) Clear() { id.scalar.Clear() }

// SafetyNumber is twelve 5-digit groups derived from both public keys.
type SafetyNumber [12]uint32

const safetyPrefix = "burrow/1 safety"

// ComputeSafetyNumber is symmetric in its arguments (§3.3).
func ComputeSafetyNumber(a, b PeerID) SafetyNumber {
	lo, hi := a, b
	if string(hi[:]) < string(lo[:]) {
		lo, hi = hi, lo
	}
	h, _ := blake2b.New512(nil)
	h.Write([]byte(safetyPrefix))
	h.Write(lo[:])
	h.Write(hi[:])
	sum := h.Sum(nil)
	var sn SafetyNumber
	for i := range sn {
		blk := sum[i*5 : i*5+5]
		v := uint64(blk[0])<<32 | uint64(binary.BigEndian.Uint32(blk[1:]))
		sn[i] = uint32(v % 100000)
	}
	return sn
}

// String renders the safety number as "12345 67890 ..." (12 groups).
func (s SafetyNumber) String() string {
	var sb strings.Builder
	sb.Grow(12*6 - 1)
	for i, g := range s {
		if i > 0 {
			sb.WriteByte(' ')
		}
		d := [5]byte{}
		for j := 4; j >= 0; j-- {
			d[j] = byte('0' + g%10)
			g /= 10
		}
		sb.Write(d[:])
	}
	return sb.String()
}

// MDNSTag is the keyed announcement hash for opt-in LAN discovery (§5):
// BLAKE2b-256("burrow/1 mdns" || nonce || pubkey)[:16].
func MDNSTag(nonce [16]byte, pk PeerID) [16]byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte("burrow/1 mdns"))
	h.Write(nonce[:])
	h.Write(pk[:])
	var tag [16]byte
	copy(tag[:], h.Sum(nil))
	return tag
}
