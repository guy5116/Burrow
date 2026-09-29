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

// Engine is the chat engine.
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
	done       chan struct{}

	replay   *handshake.ReplayLRU
	limiters map[transport.Kind]*handshake.FailureLimiter

	mu        sync.Mutex
	started   bool
	contacts  map[PeerID]*Contact
	invites   map[[16]byte]*inviteRec
	reserved  map[[16]byte]PeerID // single-use tokens reserved at msg3
	peers     map[PeerID]*peer
	pending   int // handshakes in progress
	listeners []transport.Listener
	// orphanQueues holds a contact's message queue while no session is live.
	orphanQueues map[PeerID][]*queued
	reconnects   map[PeerID]context.CancelFunc
	transfers    map[TransferID]*transfer
	histMu       sync.Mutex
	mdnsSeen     map[[16]byte]bool
}

// New builds an engine over an unlocked store and the given transports.
func New(cfg Config, st *store.Store, trs []transport.Transport, logger *slog.Logger) (*Engine, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	id, err := st.LoadIdentity()
	if err != nil {
		return nil, err
	}
	e := &Engine{cfg: cfg, st: st, id: id, trs: map[transport.Kind]transport.Transport{}, log: logger,
		clock: time.Now, events: make(chan Event, eventsCap), done: make(chan struct{}), replay: handshake.NewReplayLRU(),
		limiters: map[transport.Kind]*handshake.FailureLimiter{}, reserved: map[[16]byte]PeerID{}, peers: map[PeerID]*peer{},
		orphanQueues: map[PeerID][]*queued{}, reconnects: map[PeerID]context.CancelFunc{}, transfers: map[TransferID]*transfer{}}
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
	return e, nil
}

func (e *Engine) now() time.Time { return e.clock().Add(time.Duration(e.clockOffset.Load())) }

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
	e.ctx, e.cancel = context.WithCancel(ctx)
	defer e.cancel()
	e.sessCtx, e.sessCancel = context.WithCancel(context.Background())
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
	e.mu.Lock()
	e.started = true
	e.listeners = listeners
	e.mu.Unlock()
	if e.cfg.MDNS {
		e.runMDNS(e.listenPortOf())
	}
	<-e.ctx.Done()
	for _, l := range listeners {
		_ = l.Close()
	}
	e.mu.Lock()
	e.started = false
	peers := make([]*peer, 0, len(e.peers))
	for _, p := range e.peers {
		peers = append(peers, p)
	}
	e.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(p *peer) { defer wg.Done(); p.close(wire.ByeShutdown) }(p)
	}
	wg.Wait()
	e.sessCancel()
	e.wg.Wait()
	for _, tr := range e.trs {
		if c, ok := tr.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
	e.id.Clear()
	close(e.done)
	return nil
}

// Done closes once Start has returned; Events is never closed (late emits are dropped).
func (e *Engine) Done() <-chan struct{} { return e.done }

// ListenAddrs returns the bound listener addresses (valid after Start).
func (e *Engine) ListenAddrs() []net.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]net.Addr, 0, len(e.listeners))
	for _, l := range e.listeners {
		out = append(out, l.Addr())
	}
	return out
}

func (e *Engine) emit(ev Event) {
	select {
	case e.events <- ev:
	case <-e.ctx.Done():
	}
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
		e.mu.Lock()
		busy := e.pending >= wire.MaxPendingHandshakes || len(e.peers)+e.pending >= e.cfg.maxPeers()
		limited := lim.Limited(key, e.now())
		if tr.Kind() == transport.KindTor && limited {
			limited = e.pending >= wire.TorLimitedSlots
		}
		if !busy && !limited {
			e.pending++
		}
		e.mu.Unlock()
		if busy || limited {
			_ = conn.Close()
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer func() {
				e.mu.Lock()
				e.pending--
				e.mu.Unlock()
			}()
			e.serve(tr, conn, key, lim)
		}()
	}
}

// serve runs the responder side of one inbound connection.
func (e *Engine) serve(tr transport.Transport, conn net.Conn, key string, lim *handshake.FailureLimiter) {
	var token *[16]byte
	res, err := handshake.Respond(e.ctx, conn, e.id, func(p PeerID, tok *[16]byte) handshake.Decision {
		token = tok
		return e.authorize(p, tok)
	}, e.replay)
	if err != nil {
		_ = conn.Close()
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

// authorize implements §4.6 for the responder. Runs on the accept goroutine.
func (e *Engine) authorize(p PeerID, tok *[16]byte) handshake.Decision {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.contacts[p]; ok {
		if c.Blocked {
			return handshake.Reject
		}
		return handshake.Accept // a present token is ignored, never consumed
	}
	if tok == nil {
		return handshake.Reject
	}
	inv, ok := e.invites[*tok]
	if !ok || !e.now().Before(inv.Expiry) {
		return handshake.Reject
	}
	if len(e.contacts) >= wire.MaxContacts || (inv.MultiUse && inv.Uses >= wire.MaxContactsPerInvite) {
		return handshake.Reject
	}
	if !inv.MultiUse {
		if _, taken := e.reserved[*tok]; taken {
			return handshake.Reject
		}
		e.reserved[*tok] = p
	}
	return handshake.Accept
}

func (e *Engine) releaseToken(tok *[16]byte) {
	if tok == nil {
		return
	}
	e.mu.Lock()
	delete(e.reserved, *tok)
	e.mu.Unlock()
}

// Connect dials an invite or a saved contact and returns once the session is
// established (both HELLOs) or fails.
func (e *Engine) Connect(ctx context.Context, target Target) (PeerID, error) {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		return PeerID{}, ErrNotRunning
	}
	var addr transport.Address
	var peerID PeerID
	var token *[16]byte
	var inviteID string
	if target.InviteBytes != nil {
		defer clear(target.InviteBytes)
	}
	switch {
	case target.InviteBytes != nil, target.Invite != "":
		var inv *invite.Invite
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
		addr = transport.Address{Kind: kindOf(inv.Kind), Host: inv.Addr, Port: inv.Port}
		peerID = inv.PubKey
		e.mu.Lock()
		c, known := e.contacts[peerID]
		e.mu.Unlock()
		if known && c.Blocked {
			return peerID, ErrBlocked
		}
		if !known {
			e.mu.Lock()
			full := len(e.contacts) >= wire.MaxContacts
			e.mu.Unlock()
			if full {
				return peerID, ErrContactLimit
			}
			t := inv.Token
			token = &t
			inviteID = "peer"
		}
	case target.Contact != nil:
		peerID = *target.Contact
		e.mu.Lock()
		c, ok := e.contacts[peerID]
		if ok {
			if c.Blocked {
				e.mu.Unlock()
				return peerID, ErrBlocked
			}
			if len(c.Addrs) > 0 {
				addr = c.Addrs[0]
			}
		}
		e.mu.Unlock()
		if !ok {
			return peerID, ErrUnknownContact
		}
		if addr.Host == "" {
			return peerID, ErrNoAddress
		}
	default:
		return PeerID{}, errors.New("core: empty target")
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
	e.mu.Lock()
	if len(e.peers)+e.pending >= e.cfg.maxPeers() {
		e.mu.Unlock()
		return ErrTooManyPeers
	}
	e.pending++
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.pending--
		e.mu.Unlock()
	}()
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
	if err := e.establish(conn, addr.Kind, res, nil, inviteID); err != nil {
		e.emit(HandshakeFailed{Peer: peerID, Stage: "hello", Reason: reasonOf(err)})
		return err
	}
	e.rememberAddr(peerID, addr)
	return nil
}

// reasonOf is the user-facing wording for a failed HELLO exchange (§3.3).
func reasonOf(err error) string {
	if errors.Is(err, session.ErrProtocol) {
		return "protocol violation"
	}
	return "they may be offline, may have blocked you, or their key may have changed (a fresh invite would be needed)"
}

// establish wraps an authenticated conn in a session, runs the HELLO
// exchange, then applies token consumption, contact creation, name-collision
// and replacement/glare rules. token is the responder-side presented token.
func (e *Engine) establish(conn net.Conn, kind transport.Kind, res *handshake.Result, token *[16]byte, inviteID string) error {
	hello := wire.Hello{Name: []byte(e.cfg.DisplayName), Features: wire.FeatureImages | wire.FeatureAnimatedGIF, MaxImage: e.cfg.maxImage()}
	if e.cfg.Typing {
		hello.Features |= wire.FeatureTyping
	}
	s, err := session.New(session.Config{Conn: conn, Root: res.Root, Initiator: res.Initiator, Self: e.id,
		Peer: res.Peer, Hello: hello, Logger: e.log})
	if err != nil {
		_ = conn.Close()
		e.releaseToken(token)
		return err
	}
	s.Start(e.sessCtx)
	select {
	case <-s.Ready():
	case <-s.Done():
		e.releaseToken(token)
		return s.Err()
	case <-time.After(wire.HandshakeTimeout):
		s.Close(wire.ByeLocalError)
		e.releaseToken(token)
		return errors.New("core: HELLO timeout")
	}
	name, err := text.Sanitize(s.PeerHello().Name, text.Name)
	if err != nil {
		s.Close(wire.ByeLocalError)
		e.releaseToken(token)
		return err
	}
	p := newPeer(e, s, kind)

	e.mu.Lock()
	// Token consumption and contact creation (§4.6), both HELLOs validated.
	var newContact *Contact
	var collision *NameCollision
	var consumedID string
	if _, known := e.contacts[res.Peer]; !known {
		if token != nil {
			inv, ok := e.invites[*token]
			if !ok || !e.now().Before(inv.Expiry) {
				e.mu.Unlock()
				delete(e.reserved, *token)
				s.Close(wire.ByeLocalError)
				return errors.New("core: invite vanished during handshake")
			}
			inv.Uses++
			consumedID = inv.ID
			inviteID = inv.ID
			if !inv.MultiUse {
				delete(e.invites, *token)
			}
			delete(e.reserved, *token)
			_ = e.saveInvites()
		}
		if inviteID != "" || token != nil {
			// Only the side that issued the invite knows its id; the side that used
			// one records none ("peer" above only marks "create the contact").
			if res.Initiator {
				inviteID = ""
			}
			if len(e.contacts) >= wire.MaxContacts {
				e.mu.Unlock()
				s.Close(wire.ByeResourceLimit)
				return ErrContactLimit
			}
			newContact, collision = e.addContactLocked(res.Peer, name, inviteID)
		}
	} else if c := e.contacts[res.Peer]; c.Blocked {
		e.mu.Unlock()
		s.Close(wire.ByeLocalError)
		return ErrBlocked
	}
	if newContact == nil {
		if _, known := e.contacts[res.Peer]; !known {
			// Unknown peer without an invite path cannot happen (authorize rejects), but be safe.
			e.mu.Unlock()
			s.Close(wire.ByeLocalError)
			return ErrUnknownContact
		}
	}
	// Replacement / glare (§12).
	if old, ok := e.peers[res.Peer]; ok {
		if e.now().Sub(old.since) < wire.GlareWindow {
			// True simultaneous dial: keep the session whose initiator has the smaller key.
			keepNew := initiatorKey(e.id.Public(), res.Peer, res.Initiator) < initiatorKey(e.id.Public(), res.Peer, old.s.Initiator())
			if !keepNew {
				e.mu.Unlock()
				s.Close(wire.ByeReplaced)
				return errors.New("core: lost simultaneous-dial glare")
			}
		}
		old.replaced = true
		e.peers[res.Peer] = p
		e.mu.Unlock()
		old.close(wire.ByeReplaced)
		e.mu.Lock()
		if e.peers[res.Peer] != p { // lost a race with yet another session
			e.mu.Unlock()
			s.Close(wire.ByeReplaced)
			return errors.New("core: replaced")
		}
	} else {
		e.peers[res.Peer] = p
	}
	e.contacts[res.Peer].Online = true
	p.queue = append(p.queue, e.orphanQueues[res.Peer]...)
	delete(e.orphanQueues, res.Peer)
	if cancel := e.reconnects[res.Peer]; cancel != nil {
		cancel()
		delete(e.reconnects, res.Peer)
	}
	e.mu.Unlock()

	if newContact != nil {
		e.emit(NewPeerViaInvite{Peer: res.Peer, Nickname: newContact.Nickname, InviteID: inviteID})
	}
	if consumedID != "" {
		e.emit(InviteConsumed{ID: consumedID})
	}
	if collision != nil {
		e.emit(*collision)
	}
	e.emit(PeerConnected{Peer: res.Peer, Initiator: res.Initiator, Transport: kind, Name: name})
	e.wg.Add(1)
	go p.run()
	return nil
}

// initiatorKey returns the public key of the initiator of a session between us and peer.
func initiatorKey(self, peer PeerID, weInitiated bool) string {
	if weInitiated {
		return string(self[:])
	}
	return string(peer[:])
}

// addContactLocked creates a contact with the default nickname and runs the
// name-collision check. Caller holds e.mu.
func (e *Engine) addContactLocked(id PeerID, helloName, inviteID string) (*Contact, *NameCollision) {
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

func (e *Engine) rememberAddr(id PeerID, addr transport.Address) {
	e.mu.Lock()
	defer e.mu.Unlock()
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
