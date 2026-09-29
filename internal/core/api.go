package core

import (
	"context"
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
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Contact, 0, len(e.contacts))
	for _, c := range e.contacts {
		cc := *c
		cc.Addrs = append([]transport.Address(nil), c.Addrs...)
		out = append(out, cc)
	}
	return out
}

// Contact returns one contact.
func (e *Engine) Contact(id PeerID) (Contact, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.contacts[id]
	if !ok {
		return Contact{}, ErrUnknownContact
	}
	return *c, nil
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
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.contacts[id]
	if !ok {
		return ErrUnknownContact
	}
	c.Nickname = clean
	return e.saveContacts()
}

// VerifyContact marks a contact verified after an out-of-band safety-number check.
func (e *Engine) VerifyContact(id PeerID) error {
	e.mu.Lock()
	c, ok := e.contacts[id]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownContact
	}
	c.Verified = true
	err := e.saveContacts()
	e.mu.Unlock()
	if err != nil {
		return err
	}
	e.emit(PeerVerified{Peer: id})
	return nil
}

// BlockContact blocks or unblocks; blocking closes any live session.
func (e *Engine) BlockContact(id PeerID, blocked bool) error {
	e.mu.Lock()
	c, ok := e.contacts[id]
	if !ok {
		e.mu.Unlock()
		return ErrUnknownContact
	}
	c.Blocked = blocked
	err := e.saveContacts()
	p := e.peers[id]
	if blocked {
		if cancel := e.reconnects[id]; cancel != nil {
			cancel()
		}
	}
	e.mu.Unlock()
	if blocked && p != nil {
		p.close(wire.ByeUserQuit)
	}
	return err
}

// RemoveContact deletes a contact, its queue and any live session.
func (e *Engine) RemoveContact(id PeerID) error {
	e.mu.Lock()
	if _, ok := e.contacts[id]; !ok {
		e.mu.Unlock()
		return ErrUnknownContact
	}
	delete(e.contacts, id)
	delete(e.orphanQueues, id)
	if cancel := e.reconnects[id]; cancel != nil {
		cancel()
	}
	p := e.peers[id]
	err := e.saveContacts()
	e.mu.Unlock()
	if p != nil {
		p.close(wire.ByeUserQuit)
	}
	return err
}

// SafetyNumber computes the verification string for a contact.
func (e *Engine) SafetyNumber(id PeerID) (SafetyNumber, error) {
	e.mu.Lock()
	_, ok := e.contacts[id]
	e.mu.Unlock()
	if !ok {
		return SafetyNumber{}, ErrUnknownContact
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
	rec := &inviteRec{ID: id, Token: hexOf(inv.Token[:]), Kind: opts.Kind, Host: opts.Host, Port: opts.Port,
		Expiry: inv.Expiry, MultiUse: opts.MultiUse}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.invites[inv.Token] = rec
	if err := e.saveInvites(); err != nil {
		delete(e.invites, inv.Token)
		return Invite{}, err
	}
	return Invite{ID: id, String: s, Expiry: inv.Expiry, MultiUse: opts.MultiUse}, nil
}

// Invites lists unexpired issued invites (without their strings).
func (e *Engine) Invites() []Invite {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Invite, 0, len(e.invites))
	for _, r := range e.invites {
		out = append(out, Invite{ID: r.ID, Expiry: r.Expiry, MultiUse: r.MultiUse})
	}
	return out
}

// RevokeInvite deletes an invite by id.
func (e *Engine) RevokeInvite(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for tok, r := range e.invites {
		if r.ID == id {
			delete(e.invites, tok)
			return e.saveInvites()
		}
	}
	return errors.New("core: no such invite")
}

// Disconnect closes the session with a contact and cancels reconnects.
func (e *Engine) Disconnect(id PeerID) error {
	e.mu.Lock()
	p := e.peers[id]
	if cancel := e.reconnects[id]; cancel != nil {
		cancel()
	}
	e.mu.Unlock()
	if p == nil {
		return nil
	}
	p.close(wire.ByeUserQuit)
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
	e.mu.Lock()
	c, ok := e.contacts[id]
	if !ok {
		e.mu.Unlock()
		return 0, ErrUnknownContact
	}
	if c.Blocked {
		e.mu.Unlock()
		return 0, ErrBlocked
	}
	p := e.peers[id]
	if p != nil {
		if len(p.queue) >= wire.MaxQueuedMessages {
			e.mu.Unlock()
			return 0, ErrQueueFull
		}
		p.queue = append(p.queue, q)
	} else {
		if len(e.orphanQueues[id]) >= wire.MaxQueuedMessages {
			e.mu.Unlock()
			return 0, ErrQueueFull
		}
		e.orphanQueues[id] = append(e.orphanQueues[id], q)
	}
	e.mu.Unlock()
	e.recordHistory(id, q.id, true, clean, e.now())
	e.emit(MessageStatus{Peer: id, ID: q.id, Status: StatusPending})
	if p != nil {
		select {
		case p.sendCh <- q:
		default: // cannot happen: the channel is as large as the queue bound
		}
	}
	return q.id, nil
}

// SetTyping sends a typing indicator when enabled locally and supported by the peer.
func (e *Engine) SetTyping(id PeerID, typing bool) {
	if !e.cfg.Typing {
		return
	}
	e.mu.Lock()
	p := e.peers[id]
	e.mu.Unlock()
	if p == nil || p.s.PeerHello().Features&wire.FeatureTyping == 0 {
		return
	}
	st := uint8(wire.TypingStop)
	if typing {
		st = wire.TypingStart
	}
	ctx, cancel := context.WithTimeout(e.ctx, time.Second)
	defer cancel()
	_ = p.s.Send(ctx, wire.TypeTyping, wire.AppendTyping(nil, st))
}

// Queue returns the pending/sent messages for a contact (oldest first).
func (e *Engine) Queue(id PeerID) []MessageStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	var src []*queued
	if p := e.peers[id]; p != nil {
		src = p.queue
	} else {
		src = e.orphanQueues[id]
	}
	out := make([]MessageStatus, 0, len(src))
	for _, q := range src {
		out = append(out, MessageStatus{Peer: id, ID: q.id, Status: q.status})
	}
	return out
}
