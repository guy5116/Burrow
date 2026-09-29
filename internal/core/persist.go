package core

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/transport"
)

// Blob relpaths.
const (
	contactsBlob = "contacts"
	invitesBlob  = "invites"
)

// Persisted forms use JSON (stdlib, not on the wire path) inside encrypted blobs.
type contactRec struct {
	ID        string              `json:"id"`
	Nickname  string              `json:"nick"`
	Verified  bool                `json:"verified"`
	Blocked   bool                `json:"blocked"`
	Addrs     []transport.Address `json:"addrs,omitempty"`
	FirstSeen time.Time           `json:"first_seen"`
	InviteID  string              `json:"invite_id,omitempty"`
}

type inviteRec struct {
	ID       string         `json:"id"`
	Token    string         `json:"token"`
	Kind     transport.Kind `json:"kind"`
	Host     string         `json:"host"`
	Port     uint16         `json:"port"`
	Expiry   time.Time      `json:"expiry"`
	MultiUse bool           `json:"multi"`
	Uses     int            `json:"uses"`
}

func (e *Engine) loadState() error {
	e.contacts = map[PeerID]*Contact{}
	e.invites = map[[16]byte]*inviteRec{}
	var crs []contactRec
	if err := e.readJSON(contactsBlob, &crs); err != nil {
		return err
	}
	for _, r := range crs {
		id, err := hex.DecodeString(r.ID)
		if err != nil || len(id) != 32 {
			return errors.New("core: corrupt contacts blob")
		}
		var pid PeerID
		copy(pid[:], id)
		e.contacts[pid] = &Contact{ID: pid, Nickname: r.Nickname, Verified: r.Verified, Blocked: r.Blocked,
			Addrs: r.Addrs, FirstSeen: r.FirstSeen, InviteID: r.InviteID}
	}
	var irs []inviteRec
	if err := e.readJSON(invitesBlob, &irs); err != nil {
		return err
	}
	now := e.now()
	for i := range irs {
		tok, err := hex.DecodeString(irs[i].Token)
		if err != nil || len(tok) != 16 {
			return errors.New("core: corrupt invites blob")
		}
		if !now.Before(irs[i].Expiry) {
			continue // expired: drop on load
		}
		var k [16]byte
		copy(k[:], tok)
		r := irs[i]
		e.invites[k] = &r
	}
	return nil
}

func (e *Engine) readJSON(rel string, v any) error {
	b, err := e.st.ReadBlob(rel)
	if err != nil {
		if errors.Is(err, errNotExist) || isNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, v)
}

// saveContacts/saveInvites run on the engine goroutine.
func (e *Engine) saveContacts() error {
	crs := make([]contactRec, 0, len(e.contacts))
	for _, c := range e.contacts {
		crs = append(crs, contactRec{ID: hex.EncodeToString(c.ID[:]), Nickname: c.Nickname, Verified: c.Verified,
			Blocked: c.Blocked, Addrs: c.Addrs, FirstSeen: c.FirstSeen, InviteID: c.InviteID})
	}
	b, err := json.Marshal(crs)
	if err != nil {
		return err
	}
	return e.st.WriteBlob(contactsBlob, b)
}

func (e *Engine) saveInvites() error {
	irs := make([]inviteRec, 0, len(e.invites))
	for _, r := range e.invites {
		irs = append(irs, *r)
	}
	b, err := json.Marshal(irs)
	if err != nil {
		return err
	}
	return e.st.WriteBlob(invitesBlob, b)
}

var errNotExist = store.ErrNoStore
