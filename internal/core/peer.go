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

// peer is one live session plus the per-contact queue and dedup state.
type peer struct {
	e        *Engine
	s        *session.Session
	kind     transport.Kind
	since    time.Time
	replaced bool

	// Engine-mutex-guarded.
	queue     []*queued
	seen      map[MsgID]struct{}
	order     []MsgID // ring of the last MsgIDDedupSet received ids
	stop      chan struct{}
	transfers map[uint16]*transfer // by stream id
}

func newPeer(e *Engine, s *session.Session, kind transport.Kind) *peer {
	return &peer{e: e, s: s, kind: kind, since: e.now(), seen: map[MsgID]struct{}{}, stop: make(chan struct{}), transfers: map[uint16]*transfer{}}
}

// run pumps inbound frames until the session ends, then handles teardown,
// queue reversion and reconnect scheduling. Runs on its own goroutine.
func (p *peer) run() {
	defer p.e.wg.Done()
	p.flushQueue()
	for {
		select {
		case in := <-p.s.Inbound():
			p.handle(in)
			continue
		case <-p.s.Done():
		}
		break
	}
	p.onClosed()
}

func (p *peer) handle(in session.Inbound) {
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
		p.e.mu.Lock()
		_, dup := p.seen[mid]
		if !dup {
			p.seen[mid] = struct{}{}
			p.order = append(p.order, mid)
			if len(p.order) > wire.MsgIDDedupSet {
				delete(p.seen, p.order[0])
				p.order = p.order[1:]
			}
		}
		p.e.mu.Unlock()
		// ACK always (re-sent for duplicates); deliver once.
		_ = p.s.Send(p.e.sessCtx, wire.TypeAck, wire.AppendAck(nil, wire.Ack{MsgID: tx.MsgID, Status: wire.AckDelivered}))
		if dup {
			return
		}
		var at time.Time
		if tx.SentAt != 0 {
			at = time.Unix(tx.SentAt, 0)
		}
		p.e.recordHistory(id, mid, false, body, p.e.now())
		p.e.emit(MessageReceived{Peer: id, ID: mid, Text: body, SentAt: at})
	case wire.TypeAck:
		a, err := wire.DecodeAck(in.Payload)
		if err != nil {
			return
		}
		p.e.mu.Lock()
		var hit bool
		for i, q := range p.queue {
			if q.id == MsgID(a.MsgID) {
				p.queue = append(p.queue[:i], p.queue[i+1:]...)
				hit = true
				break
			}
		}
		p.e.mu.Unlock()
		if hit {
			p.e.emit(MessageStatus{Peer: id, ID: MsgID(a.MsgID), Status: StatusDelivered})
		}
	case wire.TypeTyping:
		if !p.e.cfg.Typing {
			return
		}
		st, err := wire.DecodeTyping(in.Payload)
		if err != nil {
			return
		}
		p.e.emit(Typing{Peer: id, Typing: st == wire.TypingStart})
	default:
		if in.Type.Class() == wire.ClassTransfer {
			p.e.onStreamInbound(p, in)
		}
	}
}

// flushQueue sends every pending message in order (after the HELLO exchange).
func (p *peer) flushQueue() {
	p.e.mu.Lock()
	var items []*queued
	for _, q := range p.queue {
		if q.status == StatusPending {
			items = append(items, q)
		}
	}
	p.e.mu.Unlock()
	for _, q := range items {
		if err := p.send(q); err != nil {
			return
		}
	}
}

func (p *peer) send(q *queued) error {
	payload, err := wire.AppendText(nil, wire.Text{MsgID: uint64(q.id), SentAt: q.sentAt, Text: []byte(q.text)})
	if err != nil {
		return err
	}
	if err := p.s.Send(p.e.sessCtx, wire.TypeText, payload); err != nil {
		return err
	}
	p.e.mu.Lock()
	if q.status == StatusPending {
		q.status = StatusSent
	}
	p.e.mu.Unlock()
	p.e.emit(MessageStatus{Peer: p.s.Peer(), ID: q.id, Status: StatusSent})
	return nil
}

func (p *peer) close(reason uint8) {
	p.s.Close(reason)
}

// onClosed runs once the session is done.
func (p *peer) onClosed() {
	id := p.s.Peer()
	err := p.s.Err()
	e := p.e
	e.failPeerTransfers(p)
	e.mu.Lock()
	current := e.peers[id] == p
	if current {
		delete(e.peers, id)
		if c, ok := e.contacts[id]; ok {
			c.Online = false
		}
	}
	// Every sent-but-undelivered message reverts to pending; the next session resends it.
	var reverted []MsgID
	for _, q := range p.queue {
		if q.status == StatusSent {
			q.status = StatusPending
			reverted = append(reverted, q.id)
		}
	}
	// Hand the queue to the contact's holding slot so it survives the session.
	if current {
		e.orphanQueues[id] = append(e.orphanQueues[id], p.queue...)
	}
	c, known := e.contacts[id]
	var addrs []transport.Address
	if known {
		addrs = c.Addrs
	}
	// A BYE "replaced" from the peer means a newer session exists (ours or theirs): not a disconnect.
	var be *session.ByeError
	replaced := p.replaced || (errors.As(err, &be) && be.Reason == wire.ByeReplaced)
	e.mu.Unlock()
	for _, mid := range reverted {
		e.emit(MessageStatus{Peer: id, ID: mid, Status: StatusPending})
	}
	if replaced {
		return // the replacing session already emitted PeerConnected; no disconnect event
	}
	e.emit(PeerDisconnected{Peer: id, Reason: disconnectReason(err)})
	if current && known && e.cfg.AutoReconnect && len(addrs) > 0 && e.ctx.Err() == nil && shouldReconnect(err) {
		e.scheduleReconnect(id)
	}
}

// shouldReconnect: only after a lost connection or a BYE the peer did not
// choose (local error, resource limit). A deliberate BYE, our own close, or a
// protocol violation never triggers reconnect beacons.
func shouldReconnect(err error) bool {
	var be *session.ByeError
	switch {
	case err == nil, errors.Is(err, session.ErrClosed), errors.Is(err, session.ErrProtocol):
		return false
	case errors.As(err, &be):
		return be.Reason == wire.ByeLocalError || be.Reason == wire.ByeResourceLimit
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

// scheduleReconnect starts (or keeps) one reconnect loop per contact.
func (e *Engine) scheduleReconnect(id PeerID) {
	e.mu.Lock()
	if _, running := e.reconnects[id]; running {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.reconnects[id] = cancel
	e.mu.Unlock()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			if e.reconnects[id] != nil {
				delete(e.reconnects, id)
			}
			e.mu.Unlock()
		}()
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
			e.mu.Lock()
			c, ok := e.contacts[id]
			_, online := e.peers[id]
			var addr transport.Address
			if ok && len(c.Addrs) > 0 {
				addr = c.Addrs[0]
			}
			blocked := ok && c.Blocked
			e.mu.Unlock()
			if !ok || online || blocked || addr.Host == "" {
				return
			}
			if err := e.dial(ctx, addr, id, nil, ""); err == nil {
				return
			}
			if backoff < wire.ReconnectMaxBackoff {
				backoff *= 2
				if backoff > wire.ReconnectMaxBackoff {
					backoff = wire.ReconnectMaxBackoff
				}
			}
		}
	}()
}
