package core

import (
	"errors"
	"net"
	"strings"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/text"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

// Contacts returns a snapshot of the contact list.
func (e *Engine) Contacts() []Contact {
	var out []Contact
	e.do(func() {
		out = make([]Contact, 0, len(e.contacts))
		for id := range e.contacts {
			out = append(out, e.contactL(id))
		}
	})
	return out
}

func (e *Engine) contactL(id PeerID) Contact {
	cc := *e.contacts[id]
	cc.Addrs = append([]transport.Address(nil), cc.Addrs...)
	if p := e.peers[id]; p != nil {
		cc.Transport = p.kind
	}
	return cc
}

// Contact returns one contact.
func (e *Engine) Contact(id PeerID) (Contact, error) {
	var cc Contact
	err := ErrUnknownContact
	e.do(func() {
		if _, ok := e.contacts[id]; ok {
			cc, err = e.contactL(id), nil
		}
	})
	return cc, err
}

// editContact applies fn to a contact on the engine goroutine and saves the
// list. It returns the contact's live peer, if any.
func (e *Engine) editContact(id PeerID, fn func(c *Contact)) (*peer, error) {
	var p *peer
	err := ErrUnknownContact
	e.do(func() {
		c, ok := e.contacts[id]
		if !ok {
			return
		}
		p = e.peers[id]
		fn(c)
		err = e.saveContacts()
	})
	return p, err
}

// RenameContact sets a user-chosen nickname (sanitized, single line, ≤ 64 chars).
func (e *Engine) RenameContact(id PeerID, name string) error {
	clean, err := text.Sanitize([]byte(name), text.Name)
	if err != nil {
		return err
	}
	clean = strings.TrimSpace(clean)
	if clean == "" || len(clean) > 64 {
		return errors.New("core: nickname must be 1–64 characters")
	}
	_, err = e.editContact(id, func(c *Contact) { c.Nickname = clean })
	return err
}

// VerifyContact marks a contact verified after an out-of-band safety-number check.
func (e *Engine) VerifyContact(id PeerID) error {
	_, err := e.editContact(id, func(c *Contact) {
		c.Verified = true
		e.queueL(PeerVerified{Peer: id})
	})
	return err
}

// BlockContact blocks or unblocks; blocking closes any live session.
func (e *Engine) BlockContact(id PeerID, blocked bool) error {
	p, err := e.editContact(id, func(c *Contact) {
		c.Blocked = blocked
		if blocked {
			e.stopReconnectL(id)
		}
	})
	if blocked && p != nil {
		p.s.Close(wire.ByeUserQuit)
	}
	return err
}

// RemoveContact deletes a contact, its queue and any live session.
func (e *Engine) RemoveContact(id PeerID) error {
	p, err := e.editContact(id, func(*Contact) {
		delete(e.contacts, id)
		delete(e.orphanQueues, id)
		delete(e.dedup, id)
		if p := e.peers[id]; p != nil {
			p.queue = nil
		}
		e.stopReconnectL(id)
	})
	if p != nil {
		p.s.Close(wire.ByeUserQuit)
	}
	return err
}

// SafetyNumber computes the verification string for a contact.
func (e *Engine) SafetyNumber(id PeerID) (SafetyNumber, error) {
	if _, err := e.Contact(id); err != nil {
		return SafetyNumber{}, err
	}
	return identity.ComputeSafetyNumber(e.id.Public(), id), nil
}

// CreateInvite issues and stores a new invite.
func (e *Engine) CreateInvite(opts InviteOptions) (Invite, error) {
	if opts.Host == "" {
		return Invite{}, errors.New("core: invite needs the address peers should dial")
	}
	if opts.Port == 0 {
		opts.Port = e.cfg.listenPort()
		for _, a := range e.ListenAddrs() { // a bound listener (e.g. port 0 in tests) wins
			if ta, ok := a.(*net.TCPAddr); ok && ta.Port > 0 {
				opts.Port = uint16(ta.Port) // #nosec G115 -- port range
				break
			}
		}
	}
	if opts.Kind == "" {
		opts.Kind = transport.KindTCP
	}
	if opts.TTL <= 0 {
		opts.TTL = wire.DefaultInviteTTL
	}
	kind := invite.KindTCP
	if opts.Kind == transport.KindTor {
		kind = invite.KindTor
	}
	inv := invite.Invite{Kind: kind, Addr: opts.Host, Port: opts.Port, PubKey: e.id.Public(),
		Expiry: e.now().Add(opts.TTL).Truncate(time.Second), MultiUse: opts.MultiUse}
	if _, err := randomFill(inv.Token[:]); err != nil {
		return Invite{}, err
	}
	s, err := inv.Encode()
	if err != nil {
		return Invite{}, err
	}
	id, err := store.RandomID(8)
	if err != nil {
		return Invite{}, err
	}
	rec := &inviteRec{ID: id, Kind: opts.Kind, Host: opts.Host, Port: opts.Port,
		Expiry: inv.Expiry, MultiUse: opts.MultiUse}
	e.do(func() {
		e.invites[inv.Token] = rec
		if err = e.saveInvites(); err != nil {
			delete(e.invites, inv.Token)
		}
	})
	if err != nil {
		return Invite{}, err
	}
	return Invite{ID: id, String: s, Expiry: inv.Expiry, MultiUse: opts.MultiUse}, nil
}

// Invites lists unexpired issued invites (without their strings).
func (e *Engine) Invites() []Invite {
	var out []Invite
	e.do(func() {
		out = make([]Invite, 0, len(e.invites))
		for _, r := range e.invites {
			out = append(out, Invite{ID: r.ID, Expiry: r.Expiry, MultiUse: r.MultiUse})
		}
	})
	return out
}

// RevokeInvite deletes an invite by id.
func (e *Engine) RevokeInvite(id string) error {
	err := errors.New("core: no such invite")
	e.do(func() {
		for tok, r := range e.invites {
			if r.ID == id {
				delete(e.invites, tok)
				err = e.saveInvites()
				return
			}
		}
	})
	return err
}

// Disconnect closes the session with a contact and cancels reconnects.
func (e *Engine) Disconnect(id PeerID) error {
	var p *peer
	e.do(func() {
		p = e.peers[id]
		e.stopReconnectL(id)
	})
	if p != nil {
		p.s.Close(wire.ByeUserQuit)
	}
	return nil
}

// SendText queues a message for a contact; it is sent immediately when a
// session is live, otherwise after the next successful HELLO exchange.
func (e *Engine) SendText(id PeerID, msg string) (MsgID, error) {
	clean, err := text.Sanitize([]byte(msg), text.Multiline)
	if err != nil {
		return 0, err
	}
	if len(clean) == 0 || len(clean) > wire.MaxTextBytes {
		return 0, errors.New("core: message must be 1–16384 bytes")
	}
	q := &queued{id: MsgID(randUint64()), text: clean, status: StatusPending}
	if !e.cfg.NoTimestamp {
		q.sentAt = e.now().Unix()
	}
	e.do(func() {
		c, ok := e.contacts[id]
		p := e.peers[id]
		switch {
		case !ok:
			err = ErrUnknownContact
		case c.Blocked:
			err = ErrBlocked
		case p != nil && len(p.queue) >= wire.MaxQueuedMessages, p == nil && len(e.orphanQueues[id]) >= wire.MaxQueuedMessages:
			err = ErrQueueFull
		case p != nil:
			p.queue = append(p.queue, q)
			e.queueL(MessageStatus{Peer: id, ID: q.id, Status: StatusPending})
			e.sendPendingL(p)
		default:
			e.orphanQueues[id] = append(e.orphanQueues[id], q)
			e.queueL(MessageStatus{Peer: id, ID: q.id, Status: StatusPending})
		}
	})
	if err != nil {
		return 0, err
	}
	e.do(func() { e.recordHistoryL(id, q.id, true, clean, e.now()) })
	return q.id, nil
}

// TypingEnabled reports whether typing indicators are turned on in the
// settings. Without that, SetTyping does nothing and none are shown.
func (e *Engine) TypingEnabled() bool { return e.cfg.Typing }

// SetTyping sends a typing indicator when enabled locally and supported by
// the peer. It is dropped when the chat queue is full.
func (e *Engine) SetTyping(id PeerID, typing bool) {
	if !e.cfg.Typing {
		return
	}
	st := uint8(wire.TypingStop)
	if typing {
		st = wire.TypingStart
	}
	e.do(func() {
		if p := e.peers[id]; p != nil && p.s.PeerHello().Features&wire.FeatureTyping != 0 {
			_, _ = p.s.TrySend(wire.TypeTyping, wire.AppendTyping(nil, st))
		}
	})
}

// Queue returns the pending/sent messages for a contact (oldest first).
func (e *Engine) Queue(id PeerID) []MessageStatus {
	var out []MessageStatus
	e.do(func() {
		src := e.orphanQueues[id]
		if p := e.peers[id]; p != nil {
			src = p.queue
		}
		out = make([]MessageStatus, 0, len(src))
		for _, q := range src {
			out = append(out, MessageStatus{Peer: id, ID: q.id, Status: q.status})
		}
	})
	return out
}

// InviteInfo is what a UI may show about an invite before connecting.
type InviteInfo struct {
	Host     string
	Port     uint16
	Kind     transport.Kind
	Peer     PeerID
	Expiry   time.Time
	MultiUse bool
	// Hostname is true when Host is a DNS name: resolving it tells the local
	// resolver which host the user is contacting.
	Hostname bool
}

// DescribeInvite parses an invite without any network action.
func DescribeInvite(s string) (InviteInfo, error) { return DescribeInviteBytes([]byte(s)) }

// DescribeInviteBytes is DescribeInvite for an invite held in bytes (not modified).
func DescribeInviteBytes(b []byte) (InviteInfo, error) {
	inv, err := invite.ParseBytes(b)
	if err != nil {
		return InviteInfo{}, err
	}
	info := InviteInfo{Host: inv.Addr, Port: inv.Port, Kind: kindOf(inv.Kind), Peer: inv.PubKey, Expiry: inv.Expiry, MultiUse: inv.MultiUse}
	info.Hostname = info.Kind == transport.KindTCP && net.ParseIP(inv.Addr) == nil
	return info, nil
}
