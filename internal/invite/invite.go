// Package invite encodes and parses invite strings (CLAUDE.md §4.6). An invite
// is a bearer credential: it never appears in logs, and Invite deliberately has
// no String method. The parser is strict and rejects before any network action.
package invite

import (
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
	b = append(b, version, i.Kind, byte(len(i.Addr)))
	b = append(b, i.Addr...)
	b = binary.BigEndian.AppendUint16(b, i.Port)
	b = append(b, i.PubKey[:]...)
	b = append(b, i.Token[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(i.Expiry.Unix()))
	var flags byte
	if i.MultiUse {
		flags |= flagMulti
	}
	b = append(b, flags)
	c := check(b)
	b = append(b, c[:]...)
	return Prefix + strings.ToLower(b32.EncodeToString(b)), nil
}

// Parse strictly parses an invite string. Surrounding whitespace is trimmed;
// nothing else is tolerated. Expiry is not checked here (see Expired).
func Parse(s string) (*Invite, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, Prefix) {
		return nil, ErrInvalid
	}
	body := s[len(Prefix):]
	if len(body) < 8 || len(body) > (fixedLen+maxAddrLen)*8/5+1 || body != strings.ToLower(body) {
		return nil, ErrInvalid
	}
	b, err := b32.DecodeString(strings.ToUpper(body))
	if err != nil || strings.ToLower(b32.EncodeToString(b)) != body {
		return nil, ErrInvalid
	}
	if len(b) < fixedLen || b[0] != version {
		return nil, ErrInvalid
	}
	n := int(b[2])
	if n > maxAddrLen || len(b) != fixedLen+n {
		return nil, ErrInvalid
	}
	payload, sum := b[:len(b)-checkLen], b[len(b)-checkLen:]
	want := check(payload)
	if subtle.ConstantTimeCompare(sum, want[:]) != 1 {
		return nil, ErrInvalid
	}
	inv := &Invite{Kind: b[1], Addr: string(b[3 : 3+n])}
	p := b[3+n:]
	inv.Port = binary.BigEndian.Uint16(p)
	copy(inv.PubKey[:], p[2:34])
	copy(inv.Token[:], p[34:50])
	inv.Expiry = time.Unix(int64(binary.BigEndian.Uint64(p[50:58])), 0)
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
		if net.ParseIP(i.Addr) == nil && !validHostname(i.Addr) {
			return ErrInvalid
		}
	case KindTor:
		if !strings.HasSuffix(i.Addr, onionSuffix) || len(i.Addr) != onionV3Len+len(onionSuffix) || !validHostname(i.Addr) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
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
