package core

import "github.com/guy5116/burrow/internal/invite"

func inviteParse(s string) ([16]byte, error) {
	inv, err := invite.Parse(s)
	if err != nil {
		return [16]byte{}, err
	}
	return inv.Token, nil
}
