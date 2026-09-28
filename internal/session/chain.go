// Package session runs one established Burrow session over a net.Conn:
// symmetric HMAC chains, per-frame ChaCha20-Poly1305, the rekey state machine
// and the reader/writer/controller goroutines of CLAUDE.md §2.3 and §3.5.
package session

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

// ErrEpochExhausted is returned when a chain reaches the 65,536-frame cap.
var ErrEpochExhausted = errors.New("session: epoch frame counter exhausted")

// chain is one direction of one epoch: ck advances once per frame.
type chain struct {
	ck      *secret.Buffer
	epoch   uint32
	counter uint64
	mk      [wire.ChainKeySize]byte // scratch: current message key
	tmp     [wire.ChainKeySize]byte // scratch: next chain key
	nonce   [wire.NonceSize]byte    // scratch, avoids escapes on the hot path
	ad      [wire.ADSize]byte
}

var hmacOne, hmacTwo = []byte{0x01}, []byte{0x02}

func newChain(ck *secret.Buffer, epoch uint32) *chain { return &chain{ck: ck, epoch: epoch} }

// next derives the message key for the current counter into c.mk and
// advances the chain. The caller wipes c.mk after use (wipeMK).
func (c *chain) next() error {
	if c.counter >= wire.EpochMaxFrames {
		return ErrEpochExhausted
	}
	m := hmac.New(sha256.New, c.ck.Bytes())
	m.Write(hmacOne)
	m.Sum(c.mk[:0])
	m.Reset()
	m.Write(hmacTwo)
	m.Sum(c.tmp[:0])
	copy(c.ck.Bytes(), c.tmp[:])
	secret.Wipe(c.tmp[:])
	c.counter++
	return nil
}

func (c *chain) wipeMK() { secret.Wipe(c.mk[:]) }

func (c *chain) clear() {
	if c != nil {
		c.ck.Clear()
		secret.Wipe(c.mk[:])
		secret.Wipe(c.tmp[:])
	}
}

// deriveChains: ck_i2r || ck_r2i = HKDF-Expand(sha256, root, "burrow/1 chains" || uint32(epoch), 64).
func deriveChains(root *secret.Buffer, epoch uint32) (i2r, r2i *secret.Buffer, err error) {
	info := make([]byte, 0, len(wire.ChainsInfo)+4)
	info = append(info, wire.ChainsInfo...)
	info = binary.BigEndian.AppendUint32(info, epoch)
	okm, err := hkdf.Expand(sha256.New, root.Bytes(), string(info), 2*wire.ChainKeySize)
	if err != nil {
		return nil, nil, err
	}
	i2r = secret.From(okm[:wire.ChainKeySize])
	r2i = secret.From(okm[wire.ChainKeySize:])
	return i2r, r2i, nil
}
