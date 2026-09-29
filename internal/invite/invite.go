// Package invite encodes and parses invite strings (CLAUDE.md §4.6). An invite
// is a bearer credential: it never appears in logs, and Invite deliberately has
// no String method. The parser is strict and rejects before any network action.
package invite

import (
	"bytes"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/wire"
)

// Transport kinds.
const (
	KindTCP uint8 = 1
	KindTor uint8 = 2
)

const (
	// Prefix starts every invite string.
	Prefix      = "burrow1:"
	version     = 1
	flagMulti   = 1 << 0
	checkLen    = 4
	maxAddrLen  = 253
	checkPrefix = "burrow/1 invite"
	fixedLen    = 1 + 1 + 1 + 2 + 32 + wire.InviteTokenSz + 8 + 1 + checkLen
	onionSuffix = ".onion"
	onionV3Len  = 56
)

// ErrInvalid is returned for any malformed invite; the message never contains
// the invite itself.
var ErrInvalid = errors.New("invite: invalid invite")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite is a parsed invite.
type Invite struct {
	Kind     uint8
	Addr     string // IP literal, hostname, or .onion
	Port     uint16
	PubKey   identity.PeerID
	Token    [wire.InviteTokenSz]byte
	Expiry   time.Time // second precision
	MultiUse bool
}

// Expired reports whether the invite has expired at now.
func (i *Invite) Expired(now time.Time) bool { return !now.Before(i.Expiry) }

// Encode renders the invite string. It fails only if the fields are invalid.
func (i *Invite) Encode() (string, error) {
	if err := i.validate(); err != nil {
		return "", err
	}
	b := make([]byte, 0, fixedLen+len(i.Addr))
	b = append(b, version, i.Kind, byte(len(i.Addr))) // #nosec G115 -- ≤ maxAddrLen via validate
	b = append(b, i.Addr...)
	b = binary.BigEndian.AppendUint16(b, i.Port)
	b = append(b, i.PubKey[:]...)
	b = append(b, i.Token[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(i.Expiry.Unix())) // #nosec G115 -- i64 on the wire
	var flags byte
	if i.MultiUse {
		flags |= flagMulti
	}
	b = append(b, flags)
	c := check(b)
	b = append(b, c[:]...)
	return Prefix + strings.ToLower(b32.EncodeToString(b)), nil
}

// Parse strictly parses an invite string. Prefer ParseBytes for input that
// came from a prompt, so the credential never becomes an immutable string.
func Parse(s string) (*Invite, error) { return ParseBytes([]byte(s)) }

// ParseBytes strictly parses an invite held in a byte slice. Surrounding
// whitespace is trimmed; nothing else is tolerated. Expiry is not checked here
// (see Expired). The decoded working copy is wiped before returning; wiping
// the input is the caller's job.
func ParseBytes(in []byte) (*Invite, error) {
	in = bytes.TrimSpace(in)
	if !bytes.HasPrefix(in, []byte(Prefix)) {
		return nil, ErrInvalid
	}
	body := in[len(Prefix):]
	if len(body) < 8 || len(body) > (fixedLen+maxAddrLen)*8/5+1 {
		return nil, ErrInvalid
	}
	upper := make([]byte, len(body))
	defer clear(upper)
	for i, c := range body {
		switch {
		case c >= 'a' && c <= 'z':
			upper[i] = c - 'a' + 'A'
		case c >= '2' && c <= '7':
			upper[i] = c
		default: // uppercase input, padding and anything else are rejected
			return nil, ErrInvalid
		}
	}
	b := make([]byte, b32.DecodedLen(len(upper)))
	defer clear(b)
	n, err := b32.Decode(b, upper)
	if err != nil {
		return nil, ErrInvalid
	}
	b = b[:n]
	// Canonical form only: re-encoding must give the same text (no stray trailing bits).
	again := make([]byte, b32.EncodedLen(n))
	defer clear(again)
	b32.Encode(again, b)
	if subtle.ConstantTimeCompare(again, upper) != 1 {
		return nil, ErrInvalid
	}
	if len(b) < fixedLen || b[0] != version {
		return nil, ErrInvalid
	}
	alen := int(b[2])
	if alen > maxAddrLen || len(b) != fixedLen+alen {
		return nil, ErrInvalid
	}
	payload, sum := b[:len(b)-checkLen], b[len(b)-checkLen:]
	want := check(payload)
	if subtle.ConstantTimeCompare(sum, want[:]) != 1 {
		return nil, ErrInvalid
	}
	inv := &Invite{Kind: b[1], Addr: string(b[3 : 3+alen])}
	p := b[3+alen:]
	inv.Port = binary.BigEndian.Uint16(p)
	copy(inv.PubKey[:], p[2:34])
	copy(inv.Token[:], p[34:50])
	inv.Expiry = time.Unix(int64(binary.BigEndian.Uint64(p[50:58])), 0) // #nosec G115 -- i64 on the wire
	flags := p[58]
	if flags&^flagMulti != 0 {
		return nil, ErrInvalid
	}
	inv.MultiUse = flags&flagMulti != 0
	if err := inv.validate(); err != nil {
		return nil, err
	}
	return inv, nil
}

func check(b []byte) [checkLen]byte {
	h, _ := blake2b.New256(nil)
	h.Write([]byte(checkPrefix))
	h.Write(b)
	var c [checkLen]byte
	copy(c[:], h.Sum(nil))
	return c
}

func (i *Invite) validate() error {
	if i.Port == 0 || len(i.Addr) == 0 || len(i.Addr) > maxAddrLen {
		return ErrInvalid
	}
	if i.PubKey == (identity.PeerID{}) || i.Expiry.Unix() <= 0 {
		return ErrInvalid
	}
	switch i.Kind {
	case KindTCP:
		if net.ParseIP(i.Addr) == nil && !dialableHostname(i.Addr) {
			return ErrInvalid
		}
	case KindTor:
		if !validOnion(i.Addr) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

// dialableHostname accepts a hostname that may be handed to the system
// resolver. An onion name must never be (it would reveal the service to the
// resolver), and a name whose last label is all digits is a mistyped IP
// address, not a host.
func dialableHostname(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	return validHostname(h) && !strings.HasSuffix(h, onionSuffix) && strings.Trim(last, "0123456789") != ""
}

// validOnion accepts a v3 onion address: 56 base32 characters and ".onion".
func validOnion(h string) bool {
	name, ok := strings.CutSuffix(h, onionSuffix)
	return ok && len(name) == onionV3Len && strings.Trim(name, "abcdefghijklmnopqrstuvwxyz234567") == ""
}

// validHostname accepts lowercase RFC 1123 hostnames: dot-separated labels of
// [a-z0-9-], 1–63 chars, not starting or ending with '-'.
func validHostname(h string) bool {
	if len(h) == 0 || len(h) > maxAddrLen {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
