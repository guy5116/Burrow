package session

import (
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/guy5116/burrow/internal/wire"
)

// direction byte for the AEAD associated data of frames *we send*.
func (s *Session) sendDir() uint8 {
	if s.cfg.Initiator {
		return wire.DirInitiatorToResponder
	}
	return wire.DirResponderToInitiator
}

func (s *Session) recvDir() uint8 { return 1 - s.sendDir() }

// seal encrypts the inner frame at buf[4:4+n] in place under c and writes the
// length prefix; it returns the outer frame buf[:4+n+16].
func (s *Session) seal(c *chain, buf []byte, n int) ([]byte, error) {
	counter := c.counter
	if err := c.next(); err != nil {
		return nil, err
	}
	defer c.wipeMK()
	aead, err := chacha20poly1305.New(c.mk[:])
	if err != nil {
		return nil, err
	}
	wire.FrameNonce(&c.nonce, counter)
	ad := wire.FrameAD(c.ad[:0], c.epoch, counter, s.sendDir())
	inner := buf[wire.OuterLenSize : wire.OuterLenSize+n]
	ct := aead.Seal(inner[:0], c.nonce[:], inner, ad)
	if err := wire.PutOuterLen(buf, len(ct)); err != nil {
		return nil, err
	}
	return buf[:wire.OuterLenSize+len(ct)], nil
}

// open decrypts ciphertext in place under c and returns the inner plaintext.
func (s *Session) open(c *chain, ct []byte) ([]byte, error) {
	counter := c.counter
	if err := c.next(); err != nil {
		return nil, err
	}
	defer c.wipeMK()
	aead, err := chacha20poly1305.New(c.mk[:])
	if err != nil {
		return nil, err
	}
	wire.FrameNonce(&c.nonce, counter)
	ad := wire.FrameAD(c.ad[:0], c.epoch, counter, s.recvDir())
	pt, err := aead.Open(ct[:0], c.nonce[:], ct, ad)
	if err != nil {
		return nil, ErrProtocol
	}
	return pt, nil
}
