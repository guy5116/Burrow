package core

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guy5116/burrow/internal/handshake"
	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/session"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/text"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

// Errors returned by the API.
var (
	ErrUnknownContact = errors.New("core: unknown contact")
	ErrBlocked        = errors.New("core: contact is blocked")
	ErrQueueFull      = errors.New("core: message queue full")
	ErrNoAddress      = errors.New("core: contact has no known address")
	ErrNoTransport    = errors.New("core: no transport for this address kind")
	ErrTooManyPeers   = errors.New("core: too many peers")
	ErrContactLimit   = errors.New("core: contact limit reached")
	ErrNotRunning     = errors.New("core: engine not started")
)

const eventsCap = 256

// Engine is the chat engine. One goroutine (loop) owns all peer, session,
// contact, invite and transfer bookkeeping and the event fan-out; every other
// goroutine reaches that state by handing the engine a closure (do).
// Functions whose names end in L run on the engine goroutine and must never
// call do or emit.
type Engine struct {
	cfg   Config
	st    *store.Store
	id    *identity.Identity
	trs   map[transport.Kind]transport.Transport
	log   *slog.Logger
	clock func() time.Time
	// clockOffset lets tests move the engine's notion of now without a data race.
	clockOffset atomic.Int64
	rand        func() time.Duration              // reconnect jitter hook (tests)
	chunkHook   func(t *transfer, written uint32) // test hook: called by the file-writer after each chunk

	ctx    context.Context
	cancel context.CancelFunc
	// sessCtx outlives ctx by the BYE grace period so shutdown can say goodbye.
	sessCtx    context.Context
	sessCancel context.CancelFunc
	wg         sync.WaitGroup
	events     chan Event
	ready      chan struct{} // closed once the listeners are up
	done       chan struct{}
	doneOnce   sync.Once

	// The engine goroutine's inputs.
	cmds     chan func()           // closures from other goroutines
	inbound  chan session.Inbound  // frames from every session
	closedS  chan *session.Session // sessions that finished tearing down
	emitCh   chan Event            // events raised by other goroutines
	stop     chan struct{}
	stopOnce sync.Once
	loopDone chan struct{}
	afterMu  sync.Mutex // serializes calls made after the engine goroutine has stopped
	out      []Event    // events waiting for the UI (engine goroutine only)

	replay   *handshake.ReplayLRU
	limiters map[transport.Kind]*handshake.FailureLimiter
	histMu   sync.Mutex // history blobs on disk (not engine state)

	// Owned by the engine goroutine.
	started      bool
	stopping     bool // shutdown has begun: no new goroutine may join wg
	contacts     map[PeerID]*Contact
	invites      map[[16]byte]*inviteRec
	reserved     map[[16]byte]PeerID // single-use tokens reserved at msg3
	peers        map[PeerID]*peer
	bySession    map[*session.Session]*peer // includes replaced sessions until they close
	pending      int                        // handshakes in progress
	listeners    []transport.Listener
	orphanQueues map[PeerID][]*queued // a contact's message queue while no session is live
	reconnects   map[PeerID]*reconnect
	transfers    map[TransferID]*transfer
	dedup        map[PeerID]*dedup // received TEXT ids per contact; outlives the sessions
	mdnsSeen     map[[16]byte]bool // announcement nonces already examined
	histCh       chan histRec      // to the history writer (nil when history is off)
}

// New builds an engine over an unlocked store and the given transports and
// starts its goroutine. Call Start to go online, or Close when the engine is
// only used offline (listing contacts, creating an invite).
func New(cfg Config, st *store.Store, trs []transport.Transport, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	id, err := st.LoadIdentity()
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, st: st, id: id, trs: map[transport.Kind]transport.Transport{}, log: logger,
		clock: time.Now, events: make(chan Event, eventsCap), ready: make(chan struct{}), done: make(chan struct{}), replay: handshake.NewReplayLRU(),
		limiters: map[transport.Kind]*handshake.FailureLimiter{}, reserved: map[[16]byte]PeerID{}, peers: map[PeerID]*peer{},
		bySession: map[*session.Session]*peer{}, orphanQueues: map[PeerID][]*queued{}, reconnects: map[PeerID]*reconnect{},
		transfers: map[TransferID]*transfer{}, dedup: map[PeerID]*dedup{}, mdnsSeen: map[[16]byte]bool{},
		cmds: make(chan func()), inbound: make(chan session.Inbound, wire.ChatQueueCap), closedS: make(chan *session.Session),
		emitCh: make(chan Event), stop: make(chan struct{}), loopDone: make(chan struct{})}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.sessCtx, e.sessCancel = context.WithCancel(context.Background())
	e.rand = func() time.Duration { return time.Duration(randUint64() % uint64(time.Second)) }
	for _, tr := range trs {
		e.trs[tr.Kind()] = tr
		limit := wire.TCPFailuresPerMinute
		if tr.Kind() == transport.KindTor {
			limit = wire.TorFailuresPerMinute
		}
		e.limiters[tr.Kind()] = handshake.NewFailureLimiter(limit, time.Minute, wire.RateLimitPenalty)
	}
	if err := e.loadState(); err != nil {
		id.Clear()
		return nil, err
	}
	logger.Debug("identity loaded", "locked_memory", identity.Locked())
	if cfg.DataDir == "" {
		e.cfg.DataDir = filepath.Dir(st.Dir())
	}
	e.cleanPartials()
	if cfg.History {
		e.histCh = make(chan histRec, historyQueue)
		e.wg.Add(1)
		go e.historyWriter()
	}
	go e.loop()
	return e, nil
}

func (e *Engine) now() time.Time { return e.clock().Add(time.Duration(e.clockOffset.Load())) }

// loop is the engine goroutine.
func (e *Engine) loop() {
	defer close(e.loopDone)
	tick := time.NewTicker(wire.ControllerTick)
	defer tick.Stop()
	for {
		e.flushL()
		select {
		case fn := <-e.cmds:
			fn()
		case in := <-e.inbound:
			e.onInboundL(in)
		case s := <-e.closedS:
			e.onSessionClosedL(s)
		case ev := <-e.emitCh:
			e.out = append(e.out, ev)
		case <-tick.C:
			for _, p := range e.peers {
				e.sendPendingL(p)
			}
		case <-e.stop:
			return
		}
	}
}

// flushL hands queued events to the UI. While the UI is slow, typing notices
// are dropped and everything else waits (§2.3). The engine keeps serving
// closures meanwhile, so API calls never deadlock against a full event
// channel, but it takes no frames from peers: the stall becomes backpressure
// on the readers and on the transfer goroutines.
func (e *Engine) flushL() {
	for len(e.out) > 0 {
		ev := e.out[0]
		select {
		case e.events <- ev:
			e.out = e.out[1:]
			continue
		default:
		}
		if _, typing := ev.(Typing); typing {
			e.out = e.out[1:]
			continue
		}
		select {
		case e.events <- ev:
			e.out = e.out[1:]
		case fn := <-e.cmds:
			fn()
		case <-e.ctx.Done(): // shutting down: nobody is listening any more
			e.out = nil
			return
		}
	}
	e.out = nil
}

// do runs fn on the engine goroutine and waits for it. After the engine
// goroutine has stopped, fn runs on the caller under a lock instead.
func (e *Engine) do(fn func()) {
	done := make(chan struct{})
	select {
	case e.cmds <- func() { defer close(done); fn() }:
		<-done
	case <-e.loopDone:
		e.afterMu.Lock()
		defer e.afterMu.Unlock()
		fn()
	}
}

// spawnL starts fn as a goroutine that shutdown waits for. It refuses once
// shutdown has begun, so nothing joins the wait group while Start is waiting
// on it.
func (e *Engine) spawnL(fn func()) bool {
	if e.stopping {
		return false
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		fn()
	}()
	return true
}

// spawn is spawnL for other goroutines. first, when not nil, runs on the
// engine goroutine just before fn starts, and only if it starts.
func (e *Engine) spawn(first, fn func()) (ok bool) {
	e.do(func() {
		if ok = !e.stopping; !ok {
			return
		}
		if first != nil {
			first()
		}
		e.spawnL(fn)
	})
	return ok
}

// queueL queues an event from code running on the engine goroutine.
func (e *Engine) queueL(ev Event) { e.out = append(e.out, ev) }

// emit raises an event from any other goroutine. It blocks while the UI is
// not keeping up, which is what slows the emitting goroutine down.
func (e *Engine) emit(ev Event) {
	select {
	case e.emitCh <- ev:
	case <-e.ctx.Done():
	}
}

// Events is the bounded event channel; drain it on a dedicated goroutine.
func (e *Engine) Events() <-chan Event { return e.events }

// Identity returns our own key for display.
func (e *Engine) Identity() Identity {
	p := e.id.Public()
	return Identity{ID: p, Fingerprint: p.Fingerprint(), Display: p.Display()}
}

// Start opens listeners and runs until ctx ends, then shuts down gracefully:
// BYE to established peers (best effort), listeners closed, secrets cleared.
func (e *Engine) Start(ctx context.Context) error {
	var listeners []transport.Listener
	for kind, tr := range e.trs {
		ln, err := tr.Listen(e.ctx)
		if err != nil {
			for _, l := range listeners {
				_ = l.Close()
			}
			return fmt.Errorf("listen %s: %w", kind, err)
		}
		listeners = append(listeners, ln)
		e.wg.Add(1)
		go e.acceptLoop(tr, ln)
	}
	e.do(func() {
		e.started = true
		e.listeners = listeners
		close(e.ready)
	})
	if e.cfg.MDNS {
		e.runMDNS(e.listenPortOf())
	}
	select {
	case <-ctx.Done():
	case <-e.ctx.Done():
	}
	e.cancel()
	for _, l := range listeners {
		_ = l.Close()
	}
	var sessions []*session.Session
	e.do(func() {
		e.started, e.stopping = false, true
		for _, p := range e.peers {
			sessions = append(sessions, p.s)
		}
		for _, r := range e.reconnects {
			r.cancel()
		}
	})
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *session.Session) { defer wg.Done(); s.Close(wire.ByeShutdown) }(s)
	}
	wg.Wait()
	e.sessCancel()
	e.wg.Wait()
	e.Close()
	return nil
}

// Ready closes once Start has the listeners up.
func (e *Engine) Ready() <-chan struct{} { return e.ready }

// Close stops the engine goroutine, closes transports that need it and wipes
// the identity. Start calls it on the way out; engines that were never
// started call it directly.
func (e *Engine) Close() {
	e.cancel()
	e.sessCancel()
	e.do(func() { e.stopping = true })
	e.wg.Wait()
	e.stopOnce.Do(func() { close(e.stop) })
	<-e.loopDone
	e.doneOnce.Do(func() {
		for _, tr := range e.trs {
			if c, ok := tr.(interface{ Close() error }); ok {
				_ = c.Close()
			}
		}
		e.id.Clear()
		close(e.done)
	})
}

// Done closes once the engine has shut down; Events is never closed (late emits are dropped).
func (e *Engine) Done() <-chan struct{} { return e.done }

// ListenAddrs returns the bound listener addresses (valid after Start).
func (e *Engine) ListenAddrs() []net.Addr {
	var out []net.Addr
	e.do(func() { out = e.listenAddrsL() })
	return out
}

func (e *Engine) listenAddrsL() []net.Addr {
	out := make([]net.Addr, 0, len(e.listeners))
	for _, l := range e.listeners {
		out = append(out, l.Addr())
	}
	return out
}

func (e *Engine) acceptLoop(tr transport.Transport, ln transport.Listener) {
	defer e.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		key := tr.RateKey(conn.RemoteAddr())
		lim := e.limiters[tr.Kind()]
		limited := lim.Limited(key, e.now())
		admit := false
		e.do(func() {
			busy := e.pending >= wire.MaxPendingHandshakes || len(e.peers)+e.pending >= e.cfg.maxPeers()
			if tr.Kind() == transport.KindTor && limited {
				limited = e.pending >= wire.TorLimitedSlots
			}
			if !busy && !limited {
				e.pending++
				admit = true
			}
		})
		if !admit {
			_ = conn.Close()
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer e.do(func() { e.pending-- })
			e.serve(tr, conn, key, lim)
		}()
	}
}

// serve runs the responder side of one inbound connection.
func (e *Engine) serve(tr transport.Transport, conn net.Conn, key string, lim *handshake.FailureLimiter) {
	var token *[16]byte // the invite token this connection was admitted with, if any
	var peer PeerID
	res, err := handshake.Respond(e.ctx, conn, e.id, func(p PeerID, tok *[16]byte) handshake.Decision {
		d, used := e.authorize(p, tok)
		if used {
			token, peer = tok, p
		}
		return d
	}, e.replay)
	if err != nil {
		_ = conn.Close()
		e.releaseToken(peer, token)
		lim.Fail(key, e.now())
		e.log.Debug("inbound handshake failed", "stage", stageOf(err))
		return
	}
	if err := e.establish(conn, tr.Kind(), res, token, ""); err != nil {
		e.log.Debug("inbound session failed", "reason", disconnectReason(err)) // category only: no addresses
	}
}

func stageOf(err error) string {
	var he *handshake.Error
	if errors.As(err, &he) {
		return he.Stage
	}
	return "unknown"
}

// authorize implements §4.6 for the responder. Called on a handshake
// goroutine. used reports that p was admitted because of tok (and, for a
// single-use invite, that tok is now reserved for p). A known contact uses
// no token, whatever it presents.
func (e *Engine) authorize(p PeerID, tok *[16]byte) (d handshake.Decision, used bool) {
	d = handshake.Reject
	e.do(func() {
		if c, ok := e.contacts[p]; ok {
			if !c.Blocked {
				d = handshake.Accept // a present token is ignored, never consumed
			}
			return
		}
		if tok == nil {
			return
		}
		inv, ok := e.invites[*tok]
		if !ok || !e.now().Before(inv.Expiry) || !e.roomForL(inv) {
			return
		}
		if _, taken := e.reserved[*tok]; taken && !inv.MultiUse {
			return
		}
		if !inv.MultiUse {
			e.reserved[*tok] = p
		}
		d, used = handshake.Accept, true
	})
	return d, used
}

// roomForL reports whether one more contact may be created from inv.
func (e *Engine) roomForL(inv *inviteRec) bool {
	return len(e.contacts) < wire.MaxContacts && (!inv.MultiUse || inv.Uses < wire.MaxContactsPerInvite)
}

// releaseToken gives up the reservation p holds on tok, and no one else's.
func (e *Engine) releaseToken(p PeerID, tok *[16]byte) {
	if tok == nil {
		return
	}
	e.do(func() { e.releaseTokenL(p, tok) })
}

func (e *Engine) releaseTokenL(p PeerID, tok *[16]byte) {
	if tok != nil && e.reserved[*tok] == p {
		delete(e.reserved, *tok)
	}
}

// Connect dials an invite or a saved contact and returns once the session is
// established (both HELLOs) or fails.
func (e *Engine) Connect(ctx context.Context, target Target) (PeerID, error) {
	if target.InviteBytes != nil {
		defer clear(target.InviteBytes)
	}
	var inv *invite.Invite
	if target.InviteBytes != nil || target.Invite != "" {
		var err error
		if target.InviteBytes != nil {
			inv, err = invite.ParseBytes(target.InviteBytes)
		} else {
			inv, err = invite.Parse(target.Invite)
		}
		if err != nil {
			return PeerID{}, err
		}
		if inv.Expired(e.now()) {
			return PeerID{}, errors.New("core: invite has expired")
		}
	}
	if inv == nil && target.Contact == nil {
		return PeerID{}, errors.New("core: empty target")
	}
	var addr transport.Address
	var peerID PeerID
	var token *[16]byte
	var inviteID string
	var err error
	e.do(func() {
		if !e.started {
			err = ErrNotRunning
			return
		}
		if inv != nil {
			addr = transport.Address{Kind: kindOf(inv.Kind), Host: inv.Addr, Port: inv.Port}
			peerID = inv.PubKey
			c, known := e.contacts[peerID]
			switch {
			case known && c.Blocked:
				err = ErrBlocked
			case !known && len(e.contacts) >= wire.MaxContacts:
				err = ErrContactLimit
			case !known:
				t := inv.Token
				token = &t
				inviteID = "peer"
			}
			return
		}
		peerID = *target.Contact
		c, ok := e.contacts[peerID]
		switch {
		case !ok:
			err = ErrUnknownContact
		case c.Blocked:
			err = ErrBlocked
		case len(c.Addrs) == 0:
			err = ErrNoAddress
		default:
			addr = c.Addrs[0]
		}
	})
	if err != nil {
		return peerID, err
	}
	return peerID, e.dial(ctx, addr, peerID, token, inviteID)
}

func kindOf(k uint8) transport.Kind {
	if k == invite.KindTor {
		return transport.KindTor
	}
	return transport.KindTCP
}

// dial runs the initiator side once. inviteID is non-empty when the peer is
// new to us and a contact must be created after both HELLOs.
func (e *Engine) dial(ctx context.Context, addr transport.Address, peerID PeerID, token *[16]byte, inviteID string) error {
	tr, ok := e.trs[addr.Kind]
	if !ok {
		return ErrNoTransport
	}
	full := false
	e.do(func() {
		if len(e.peers)+e.pending >= e.cfg.maxPeers() {
			full = true
			return
		}
		e.pending++
	})
	if full {
		return ErrTooManyPeers
	}
	defer e.do(func() { e.pending-- })
	conn, err := tr.Dial(ctx, addr)
	if err != nil {
		e.emit(HandshakeFailed{Peer: peerID, Stage: "dial", Reason: "could not connect"})
		return err
	}
	res, err := handshake.Initiate(ctx, conn, e.id, peerID, token)
	if err != nil {
		_ = conn.Close()
		e.emit(HandshakeFailed{Peer: peerID, Stage: stageOf(err), Reason: "handshake failed"})
		return err
	}
	err = e.establish(conn, addr.Kind, res, nil, inviteID)
	if errors.Is(err, errLostGlare) {
		return nil // both sides dialed at once and the other session was kept: connected all the same
	}
	if err != nil {
		e.emit(HandshakeFailed{Peer: peerID, Stage: "hello", Reason: reasonOf(err)})
		return err
	}
	e.do(func() { e.rememberAddrL(peerID, addr) })
	return nil
}

// errLostGlare: a simultaneous dial was resolved in favour of the other session.
var errLostGlare = errors.New("core: lost simultaneous-dial glare")

// reasonOf is the user-facing wording for a failed HELLO exchange (§3.3).
func reasonOf(err error) string {
	if errors.Is(err, session.ErrProtocol) {
		return "protocol violation"
	}
	return "they may be offline, may have blocked you, or their key may have changed (a fresh invite would be needed)"
}

// establish wraps an authenticated conn in a session, runs the HELLO exchange
// on the calling (handshake) goroutine, then asks the engine goroutine to
// register the peer. token is the invite token the peer was admitted with
// (responder side).
func (e *Engine) establish(conn net.Conn, kind transport.Kind, res *handshake.Result, token *[16]byte, inviteID string) error {
	hello := wire.Hello{Name: []byte(e.cfg.DisplayName), Features: wire.FeatureImages | wire.FeatureAnimatedGIF, MaxImage: e.cfg.maxImage()}
	if e.cfg.Typing {
		hello.Features |= wire.FeatureTyping
	}
	if hello.MaxFile = e.cfg.maxFile(); hello.MaxFile > 0 {
		hello.Features |= wire.FeatureFiles
	}
	s, err := session.New(session.Config{Conn: conn, Root: res.Root, Initiator: res.Initiator, Self: e.id,
		Peer: res.Peer, Hello: hello, Logger: e.log, Inbound: e.inbound, OnClose: e.sessionClosed})
	if err != nil {
		_ = conn.Close()
		e.releaseToken(res.Peer, token)
		return err
	}
	s.Start(e.sessCtx)
	select {
	case <-s.Ready():
	case <-s.Done():
		e.releaseToken(res.Peer, token)
		return s.Err()
	case <-time.After(wire.HandshakeTimeout):
		s.Drop()
		e.releaseToken(res.Peer, token)
		return errors.New("core: HELLO timeout")
	}
	name, err := text.Sanitize(s.PeerHello().Name, text.Name)
	if err != nil {
		s.Drop()
		e.releaseToken(res.Peer, token)
		return err
	}
	var old *session.Session
	e.do(func() { old, err = e.registerL(s, kind, res, token, inviteID, name) })
	switch {
	case errors.Is(err, errLostGlare):
		s.Close(wire.ByeReplaced)
	case err != nil:
		s.Drop() // silently, like any connection we do not want (§4.6)
	case old != nil:
		old.Close(wire.ByeReplaced)
	}
	return err
}

// registerL applies token consumption, contact creation, the name-collision
// check and the replacement/glare rules, installs the peer and lets its frames
// flow. It returns the session this one replaces, if any.
func (e *Engine) registerL(s *session.Session, kind transport.Kind, res *handshake.Result, token *[16]byte, inviteID, name string) (*session.Session, error) {
	defer e.releaseTokenL(res.Peer, token)
	select {
	case <-s.Done(): // died after its HELLO; its OnClose has fired or will find nothing
		return nil, s.Err()
	default:
	}
	// Token consumption and contact creation (§4.6), both HELLOs validated.
	var newContact *Contact
	var collision *NameCollision
	var consumedID string
	c, known := e.contacts[res.Peer]
	switch {
	case known && c.Blocked:
		return nil, ErrBlocked
	case !known:
		if token == nil && inviteID == "" {
			return nil, ErrUnknownContact // authorize rejects these; be safe
		}
		if len(e.contacts) >= wire.MaxContacts {
			return nil, ErrContactLimit
		}
		if token != nil {
			// Checked again: handshakes that ran side by side all passed authorize.
			inv, ok := e.invites[*token]
			if !ok || !e.now().Before(inv.Expiry) || !e.roomForL(inv) {
				return nil, errors.New("core: invite used up or gone during the handshake")
			}
			inv.Uses++
			consumedID, inviteID = inv.ID, inv.ID
			if !inv.MultiUse {
				delete(e.invites, *token)
			}
			_ = e.saveInvites()
		}
		// Only the side that issued the invite knows its id; the side that used
		// one records none ("peer" only marks "create the contact").
		if res.Initiator {
			inviteID = ""
		}
		newContact, collision = e.addContactL(res.Peer, name, inviteID)
	}
	// Replacement / glare (§12).
	old := e.peers[res.Peer]
	if old != nil && losesGlare(e.id.Public(), res.Peer, res.Initiator, old.s.Initiator(), e.now().Sub(old.since)) {
		return nil, errLostGlare
	}
	p := newPeer(e, s, kind)
	e.peers[res.Peer] = p
	e.bySession[s] = p
	e.contacts[res.Peer].Online = true
	var replaced *session.Session
	if old != nil {
		old.replaced, replaced = true, old.s
		e.revertSentL(old) // unacknowledged messages move over right away
		p.queue, old.queue = old.queue, nil
	}
	p.queue = append(p.queue, e.orphanQueues[res.Peer]...)
	delete(e.orphanQueues, res.Peer)
	if r := e.reconnects[res.Peer]; r != nil {
		r.cancel()
		delete(e.reconnects, res.Peer)
	}
	if newContact != nil {
		e.queueL(NewPeerViaInvite{Peer: res.Peer, Nickname: newContact.Nickname, InviteID: inviteID})
	}
	if consumedID != "" {
		e.queueL(InviteConsumed{ID: consumedID})
	}
	if collision != nil {
		e.queueL(*collision)
	}
	e.queueL(PeerConnected{Peer: res.Peer, Initiator: res.Initiator, Transport: kind, Name: name})
	s.Attach()
	e.sendPendingL(p)
	return replaced, nil
}

// sessionClosed is the session's OnClose hook: it tells the engine goroutine.
func (e *Engine) sessionClosed(s *session.Session) {
	select {
	case e.closedS <- s:
	case <-e.loopDone:
	}
}

// losesGlare reports whether a new session gives way to the one we have. That
// is so only after a simultaneous dial: the two sessions were dialed from
// opposite sides, the old one is younger than the glare window, and its
// initiator has the smaller key. Both sides reach the same verdict. Two
// sessions from the same initiator are a redial, and the newer one wins.
func losesGlare(self, peer PeerID, newInitiator, oldInitiator bool, oldAge time.Duration) bool {
	return newInitiator != oldInitiator && oldAge < wire.GlareWindow &&
		initiatorKey(self, peer, oldInitiator) < initiatorKey(self, peer, newInitiator)
}

// initiatorKey returns the public key of the initiator of a session between us and peer.
func initiatorKey(self, peer PeerID, weInitiated bool) string {
	if weInitiated {
		return string(self[:])
	}
	return string(peer[:])
}

// addContactL creates a contact with the default nickname and runs the
// name-collision check.
func (e *Engine) addContactL(id PeerID, helloName, inviteID string) (*Contact, *NameCollision) {
	nick := helloName
	if nick == "" {
		nick = id.Short()
	}
	var coll *NameCollision
	if helloName != "" {
		norm := text.Normalize(helloName)
		for _, c := range e.contacts {
			if c.ID != id && text.Normalize(c.Nickname) == norm {
				coll = &NameCollision{New: id, Existing: c.ID, Name: helloName}
				nick = helloName + " (" + id.Short() + ")"
				break
			}
		}
	}
	c := &Contact{ID: id, Nickname: nick, FirstSeen: e.now(), InviteID: inviteID}
	e.contacts[id] = c
	if err := e.saveContacts(); err != nil {
		e.log.Error("save contacts", "err", err.Error())
	}
	return c, coll
}

func (e *Engine) rememberAddrL(id PeerID, addr transport.Address) {
	c, ok := e.contacts[id]
	if !ok {
		return
	}
	for i, a := range c.Addrs {
		if a == addr {
			c.Addrs = append([]transport.Address{addr}, append(c.Addrs[:i:i], c.Addrs[i+1:]...)...)
			_ = e.saveContacts()
			return
		}
	}
	c.Addrs = append([]transport.Address{addr}, c.Addrs...)
	if len(c.Addrs) > 4 {
		c.Addrs = c.Addrs[:4]
	}
	_ = e.saveContacts()
}

func hexOf(b []byte) string { return fmt.Sprintf("%x", b) }

func randomFill(b []byte) (int, error) { return rand.Read(b) }

func randUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return binary.BigEndian.Uint64(b[:])
}

func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
