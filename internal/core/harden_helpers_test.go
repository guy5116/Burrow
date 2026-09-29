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
