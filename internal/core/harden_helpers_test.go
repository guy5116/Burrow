package core

import (
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/media"
)

func inviteParse(s string) ([16]byte, error) {
	inv, err := invite.Parse(s)
	if err != nil {
		return [16]byte{}, err
	}
	return inv.Token, nil
}

func mediaHash(b []byte) (h [32]byte) {
	hh := media.NewHasher()
	hh.Write(b)
	copy(h[:], hh.Sum(nil))
	return h
}

// peerOf returns the live session record for a contact (nil when offline).
func (n *node) peerOf(id PeerID) (p *peer) {
	n.e.do(func() { p = n.e.peers[id] })
	return p
}

func (n *node) peerCount() (c int) {
	n.e.do(func() { c = len(n.e.peers) })
	return c
}
