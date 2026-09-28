package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

// Config describes one session. Root's ownership transfers to the session.
type Config struct {
	Conn      net.Conn
	Root      *secret.Buffer
	Initiator bool
	Self      *identity.Identity // static key, for rekey DHs
	Peer      identity.PeerID
	Hello     wire.Hello // our HELLO
	Logger    *slog.Logger
	// Now and Tick are test hooks; zero values mean time.Now and 1 s.
	Now  func() time.Time
	Tick time.Duration
}

// Inbound is an application frame delivered to the owner. Payload is a copy.
type Inbound struct {
	Type    wire.FrameType
	Stream  uint16
	Payload []byte
}

// Errors visible to the owner. Protocol violations are reported as
// ErrProtocol wrapped with a redacted reason; the peer never receives BYE.
var (
	ErrProtocol  = errors.New("session: protocol violation")
	ErrClosed    = errors.New("session: closed")
	ErrQueueFull = errors.New("session: queue full")
	ErrNotReady  = errors.New("session: HELLO exchange not complete")
	ErrPeerBye   = errors.New("session: peer sent BYE")
)

// ByeError carries the peer's BYE reason.
type ByeError struct{ Reason uint8 }

func (e *ByeError) Error() string        { return fmt.Sprintf("session: peer sent BYE reason=%d", e.Reason) }
func (e *ByeError) Is(target error) bool { return target == ErrPeerBye }

// Rekey sub-states of the responder's reader.
const (
	rekeyIdle uint32 = iota
	rekeyAwaitingDone
)

// pendingRekey is the initiator's in-flight ephemerals (writer → reader).
type pendingRekey struct {
	escalar *secret.Buffer
	kemSeed *secret.Buffer
}

func (p *pendingRekey) clear() {
	p.escalar.Clear()
	p.kemSeed.Clear()
}

// ctrl is an entry on the writer's control queue.
type ctrl struct {
	kind    ctrlKind
	payload []byte         // reply frame payload (PONG nonce, REKEY_RESP)
	chain   *secret.Buffer // new SEND chain key for REKEY_RESP / REKEY_DONE
	reason  uint8          // BYE
}

type ctrlKind uint8

const (
	ctrlStartRekey ctrlKind = iota + 1
	ctrlSendPing
	ctrlSendRekeyRequest
	ctrlPong
	ctrlRekeyResp
	ctrlRekeyDone
	ctrlBye
)

type outFrame struct {
	typ     wire.FrameType
	stream  uint16
	payload []byte
}

// Session is one live connection to one peer.
type Session struct {
	cfg  Config
	conn net.Conn
	log  *slog.Logger
	now  func() time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// Writer-owned, published atomically.
	sendCounter    atomic.Uint64
	sendSwitchedAt atomic.Int64 // unix nanos
	lastWrittenAt  atomic.Int64
	pingNonce      atomic.Uint64
	rekeyInFlight  atomic.Bool
	rekeyStartedAt atomic.Int64
	requestSentAt  atomic.Int64 // 0 = unset

	// Reader-owned, published atomically.
	recvCounter      atomic.Uint64
	recvSwitchedAt   atomic.Int64
	pongNonce        atomic.Uint64
	readerRekeyState atomic.Uint32
	readerStateSince atomic.Int64
	sendEpoch        atomic.Uint32
	recvEpoch        atomic.Uint32

	// Reader-private key material.
	root     *secret.Buffer
	recv     *chain
	nextRecv *chain // responder: derived at INIT, installed at DONE
	// Writer-private.
	send *chain

	pending chan *pendingRekey // cap 1, writer → reader

	control chan ctrl
	chat    chan outFrame
	inbound chan Inbound

	peerHello wire.Hello
	helloRecv chan struct{} // closed when the peer's HELLO validated
	readyCh   chan struct{} // closed when both HELLOs are done (ours written, theirs validated)
	ready     atomic.Bool

	closeOnce sync.Once
	closed    chan struct{}
	errMu     sync.Mutex
	err       error
	byeSent   atomic.Bool

	// TYPING rate limiting (reader).
	typingTimes []time.Time
	pingTimes   []time.Time
}

// New prepares a session; Start runs it.
func New(cfg Config) (*Session, error) {
	if cfg.Conn == nil || cfg.Root == nil || cfg.Self == nil {
		return nil, errors.New("session: incomplete config")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tick == 0 {
		cfg.Tick = wire.ControllerTick
	}
	i2r, r2i, err := deriveChains(cfg.Root, 0)
	if err != nil {
		return nil, err
	}
	s := &Session{
		cfg: cfg, conn: cfg.Conn, log: cfg.Logger, now: cfg.Now,
		root:      cfg.Root,
		pending:   make(chan *pendingRekey, 1),
		control:   make(chan ctrl, wire.ControlQueueCap),
		chat:      make(chan outFrame, wire.ChatQueueCap),
		inbound:   make(chan Inbound, wire.ChatQueueCap),
		helloRecv: make(chan struct{}),
		readyCh:   make(chan struct{}),
		closed:    make(chan struct{}),
	}
	if cfg.Initiator {
		s.send, s.recv = newChain(i2r, 0), newChain(r2i, 0)
	} else {
		s.send, s.recv = newChain(r2i, 0), newChain(i2r, 0)
	}
	n := s.now().UnixNano()
	s.sendSwitchedAt.Store(n)
	s.recvSwitchedAt.Store(n)
	s.lastWrittenAt.Store(n)
	return s, nil
}

// Start launches the reader, writer and controller. It returns immediately;
// Ready() closes when both HELLOs validated, Done() when the session ends.
func (s *Session) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.spawn("reader", s.readLoop)
	s.spawn("writer", s.writeLoop)
	s.spawn("controller", s.controlLoop)
	// Tear down everything once any goroutine exits or the context ends.
	go func() {
		<-s.ctx.Done()
		s.teardown()
	}()
}

// spawn is the supervisor: a panic becomes a redacted log line and a clean
// teardown of this session only.
func (s *Session) spawn(name string, fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("session goroutine panic", "goroutine", name, "stack", string(debug.Stack()))
				s.fail(fmt.Errorf("session: panic in %s", name))
			}
		}()
		fn()
	}()
}

// fail records the first error and starts teardown. Protocol violations never send BYE.
func (s *Session) fail(err error) {
	if s.byeSent.Load() {
		err = ErrClosed // anything after our BYE is just the close
	}
	s.errMu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.errMu.Unlock()
	s.cancel()
}

func (s *Session) teardown() {
	s.closeOnce.Do(func() {
		_ = s.conn.Close()
		s.wg.Wait()
		// Clear every secret still in flight.
		select {
		case p := <-s.pending:
			p.clear()
		default:
		}
		for {
			select {
			case c := <-s.control:
				c.chain.Clear()
				continue
			default:
			}
			break
		}
		s.send.clear()
		s.recv.clear()
		s.nextRecv.clear()
		s.root.Clear()
		s.errMu.Lock()
		if s.err == nil {
			s.err = ErrClosed
		}
		s.errMu.Unlock()
		close(s.closed)
	})
}

// Close ends the session, sending BYE with reason first when the session is
// established (best effort, ByeGracePeriod).
func (s *Session) Close(reason uint8) {
	if s.ready.Load() && s.byeSent.CompareAndSwap(false, true) {
		select {
		case s.control <- ctrl{kind: ctrlBye, reason: reason}:
			select {
			case <-s.closed:
				return
			case <-time.After(wire.ByeGracePeriod):
			}
		default:
		}
	}
	s.fail(ErrClosed)
	<-s.closed
}

// Done closes when the session has fully torn down.
func (s *Session) Done() <-chan struct{} { return s.closed }

// Ready closes when both HELLOs are done and data may flow.
func (s *Session) Ready() <-chan struct{} { return s.readyCh }

// Err returns why the session ended (nil while running).
func (s *Session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.err
}

// Inbound delivers application frames (TEXT, ACK, TYPING, and in Phase 2 IMG_*).
func (s *Session) Inbound() <-chan Inbound { return s.inbound }

// PeerHello returns the validated peer HELLO (valid after Ready).
func (s *Session) PeerHello() wire.Hello { return s.peerHello }

// Peer returns the peer's key.
func (s *Session) Peer() identity.PeerID { return s.cfg.Peer }

// Initiator reports which side we are.
func (s *Session) Initiator() bool { return s.cfg.Initiator }

// Send queues a chat frame (TEXT, ACK, TYPING). It blocks under backpressure
// until ctx or the session ends. payload is copied.
func (s *Session) Send(ctx context.Context, typ wire.FrameType, payload []byte) error {
	if typ.Class() != wire.ClassChat {
		return wire.ErrStream
	}
	if !s.ready.Load() {
		return ErrNotReady
	}
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	f := outFrame{typ: typ, stream: wire.StreamChat, payload: append([]byte(nil), payload...)}
	select {
	case s.chat <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return ErrClosed
	}
}

// Rekey asks the writer to start a rekey now (initiator) or to send
// REKEY_REQUEST (responder). Tests and the controller use it.
func (s *Session) Rekey() error {
	kind := ctrlStartRekey
	if !s.cfg.Initiator {
		kind = ctrlSendRekeyRequest
	}
	return s.pushControl(ctrl{kind: kind})
}

func (s *Session) pushControl(c ctrl) error {
	select {
	case s.control <- c:
		return nil
	default:
		return ErrQueueFull
	}
}

// Epochs reports the current send and receive epochs (tests).
func (s *Session) Epochs() (send, recv uint32) {
	return uint32(s.sendEpoch.Load()), uint32(s.recvEpoch.Load())
}
