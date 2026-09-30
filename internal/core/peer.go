package core

import (
	"context"
	"errors"
	"time"

	"github.com/guy5116/burrow/internal/session"
	"github.com/guy5116/burrow/internal/text"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

// queued is one outbound TEXT with its delivery state.
type queued struct {
	id     MsgID
	text   string
	sentAt int64
	status Status
}

// maxPendingAcks bounds ACKs waiting for room in the chat queue. A peer that
// floods TEXT without reading loses ACKs beyond this; it resends after
// reconnecting and the dedup set makes that harmless.
const maxPendingAcks = 1024

// peer is the engine's record of one live session. It is owned by the engine
// goroutine; there is no goroutine per peer in core.
type peer struct {
	s        *session.Session
	kind     transport.Kind
	since    time.Time
	replaced bool // a newer session has taken this one's place

	queue     []*queued
	acks      []uint64             // ACKs not yet queued on the session
	transfers map[uint16]*transfer // by stream id
}

func newPeer(e *Engine, s *session.Session, kind transport.Kind) *peer {
	return &peer{s: s, kind: kind, since: e.now(), transfers: map[uint16]*transfer{}}
}

// dedup remembers the last MsgIDDedupSet TEXT ids received from one contact,
// so a message that is sent again after a reconnect is shown once (§4.4).
type dedup struct {
	seen  map[MsgID]struct{}
	order []MsgID // oldest first
}

// seenL records id for the contact and reports whether it was there already.
func (e *Engine) seenL(contact PeerID, id MsgID) bool {
	d := e.dedup[contact]
	if d == nil {
		d = &dedup{seen: map[MsgID]struct{}{}}
		e.dedup[contact] = d
	}
	if _, dup := d.seen[id]; dup {
		return true
	}
	d.seen[id] = struct{}{}
	d.order = append(d.order, id)
	if len(d.order) > wire.MsgIDDedupSet {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return false
}

// reconnect is one running reconnect loop.
type reconnect struct{ cancel context.CancelFunc }

// onInboundL handles one frame from a registered session.
func (e *Engine) onInboundL(in session.Inbound) {
	p := e.bySession[in.From]
	if p == nil {
		return
	}
	id := p.s.Peer()
	switch in.Type {
	case wire.TypeText:
		tx, err := wire.DecodeText(in.Payload)
		if err != nil {
			return
		}
		body, err := text.Sanitize(tx.Text, text.Multiline)
		if err != nil {
			return
		}
		mid := MsgID(tx.MsgID)
		dup := e.seenL(id, mid)
		// ACK always (re-sent for duplicates); deliver once.
		if len(p.acks) < maxPendingAcks {
			p.acks = append(p.acks, tx.MsgID)
		}
		e.sendPendingL(p)
		if dup {
			return
		}
		var at time.Time
		if tx.SentAt != 0 {
			at = time.Unix(tx.SentAt, 0)
		}
		e.recordHistoryL(id, mid, false, body, e.now())
		e.queueL(MessageReceived{Peer: id, ID: mid, Text: body, SentAt: at})
	case wire.TypeAck:
		a, err := wire.DecodeAck(in.Payload)
		if err != nil {
			return
		}
		for i, q := range p.queue {
			if q.id == MsgID(a.MsgID) {
				p.queue = append(p.queue[:i], p.queue[i+1:]...)
				e.queueL(MessageStatus{Peer: id, ID: MsgID(a.MsgID), Status: StatusDelivered})
				break
			}
		}
		e.sendPendingL(p) // an ACK means the peer is reading: there may be room again
	case wire.TypeTyping:
		if !e.cfg.Typing {
			return
		}
		st, err := wire.DecodeTyping(in.Payload)
		if err != nil {
			return
		}
		e.queueL(Typing{Peer: id, Typing: st == wire.TypingStart})
	default:
		if in.Type.Class() == wire.ClassTransfer {
			e.onStreamInboundL(p, in)
		}
	}
}

// sendPendingL moves waiting ACKs and pending messages onto the session's
// chat queue, in order, without ever blocking the engine: what does not fit
// stays here and is tried again on the next tick or the next ACK.
func (e *Engine) sendPendingL(p *peer) {
	for len(p.acks) > 0 {
		ok, err := p.s.TrySend(wire.TypeAck, wire.AppendAck(nil, wire.Ack{MsgID: p.acks[0], Status: wire.AckDelivered}))
		if err != nil || !ok {
			return
		}
		p.acks = p.acks[1:]
	}
	for _, q := range p.queue {
		if q.status != StatusPending {
			continue
		}
		payload, err := wire.AppendText(nil, wire.Text{MsgID: uint64(q.id), SentAt: q.sentAt, Text: []byte(q.text)})
		if err != nil {
			q.status = StatusFailed
			e.queueL(MessageStatus{Peer: p.s.Peer(), ID: q.id, Status: StatusFailed})
			continue
		}
		ok, err := p.s.TrySend(wire.TypeText, payload)
		if err != nil || !ok {
			return // keep the order: nothing later may overtake this one
		}
		q.status = StatusSent
		e.queueL(MessageStatus{Peer: p.s.Peer(), ID: q.id, Status: StatusSent})
	}
}

// onSessionClosedL runs once a session has fully torn down.
func (e *Engine) onSessionClosedL(s *session.Session) {
	p := e.bySession[s]
	if p == nil {
		return // never registered (the HELLO exchange failed)
	}
	delete(e.bySession, s)
	id := s.Peer()
	err := s.Err()
	e.failPeerTransfersL(p)
	if e.peers[id] != p {
		return // replaced by a newer session, which has the queue: not a disconnect
	}
	delete(e.peers, id)
	c, known := e.contacts[id]
	if !known {
		return // the contact was removed; its queue went with it
	}
	c.Online = false
	// Every sent-but-undelivered message reverts to pending and waits for the
	// next session.
	e.revertSentL(p)
	e.orphanQueues[id] = append(e.orphanQueues[id], p.queue...)
	p.queue = nil
	// The peer replaced this session with one that we are still setting up
	// (its BYE overtook our own registration): not a disconnect, unless
	// nothing comes of the dial.
	var bye *session.ByeError
	if errors.As(err, &bye) && bye.Reason == wire.ByeReplaced && e.dialing[id] > 0 {
		e.heldBack[id] = true
		return
	}
	e.disconnectedL(id, err)
}

// disconnectedL reports that the contact's session has ended and starts
// reconnecting where that is wanted.
func (e *Engine) disconnectedL(id PeerID, err error) {
	e.queueL(PeerDisconnected{Peer: id, Reason: disconnectReason(err)})
	c := e.contacts[id]
	if c != nil && e.cfg.AutoReconnect && len(c.Addrs) > 0 && !c.Blocked && !e.stopping && shouldReconnect(err) {
		e.scheduleReconnectL(id)
	}
}

// revertSentL marks p's sent but unacknowledged messages pending again.
func (e *Engine) revertSentL(p *peer) {
	for _, q := range p.queue {
		if q.status == StatusSent {
			q.status = StatusPending
			e.queueL(MessageStatus{Peer: p.s.Peer(), ID: q.id, Status: StatusPending})
		}
	}
}

// shouldReconnect: after a lost connection, or a BYE the peer did not choose:
// a local error, a resource limit, or "replaced" while this was the only
// session we had (the two sides disagreed about a simultaneous dial, §12). A
// deliberate BYE, our own close, or a protocol violation never triggers
// reconnect beacons.
func shouldReconnect(err error) bool {
	var be *session.ByeError
	switch {
	case err == nil, errors.Is(err, session.ErrClosed), errors.Is(err, session.ErrProtocol):
		return false
	case errors.As(err, &be):
		return be.Reason == wire.ByeLocalError || be.Reason == wire.ByeResourceLimit || be.Reason == wire.ByeReplaced
	}
	return true
}

func disconnectReason(err error) string {
	switch {
	case err == nil, errors.Is(err, session.ErrClosed):
		return "closed"
	case errors.Is(err, session.ErrPeerBye):
		return "peer left"
	case errors.Is(err, session.ErrProtocol):
		return "protocol violation"
	}
	return "connection lost"
}

// scheduleReconnectL starts (or keeps) one reconnect loop per contact.
func (e *Engine) scheduleReconnectL(id PeerID) {
	if e.reconnects[id] != nil {
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	r := &reconnect{cancel: cancel}
	if !e.spawnL(func() { e.reconnectLoop(ctx, id, r) }) {
		cancel()
		return
	}
	e.reconnects[id] = r
}

// stopReconnectL ends the contact's reconnect loop, if one is running.
func (e *Engine) stopReconnectL(id PeerID) {
	if r := e.reconnects[id]; r != nil {
		r.cancel()
		delete(e.reconnects, id)
	}
}

// reconnectLoop dials a contact with exponential backoff and full jitter
// until it connects, the contact goes away, or the loop is cancelled.
func (e *Engine) reconnectLoop(ctx context.Context, id PeerID, self *reconnect) {
	defer e.do(func() {
		if e.reconnects[id] == self { // a newer loop may have taken the place
			delete(e.reconnects, id)
		}
	})
	backoff := wire.ReconnectMinBackoff
	for attempt := 1; ; attempt++ {
		// Full jitter: uniform in [0, backoff].
		wait := time.Duration(float64(backoff) * float64(e.rand()) / float64(time.Second))
		e.emit(Reconnecting{Peer: id, Attempt: attempt, NextAt: e.now().Add(wait)})
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
		var addr transport.Address
		give := false
		e.do(func() {
			c, ok := e.contacts[id]
			_, online := e.peers[id]
			if !ok || online || c.Blocked || len(c.Addrs) == 0 {
				give = true
				return
			}
			addr = c.Addrs[0]
		})
		if give {
			return
		}
		if err := e.dial(ctx, addr, id, nil, ""); err == nil {
			return
		}
		if backoff < wire.ReconnectMaxBackoff {
			backoff = min(backoff*2, wire.ReconnectMaxBackoff)
		}
	}
}
