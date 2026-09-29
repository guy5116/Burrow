package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
	// RecoverInTests lets the one test of the supervisor's recovery path opt in;
	// everywhere else a panic under `go test` is re-raised and fails the test.
	RecoverInTests bool
	// Inbound, when set, receives this session's application frames instead of
	// the session's own channel, so one engine goroutine can serve every peer.
	// Nothing is delivered until Attach is called.
	Inbound chan<- Inbound
	// OnClose is called once, after the session has fully torn down.
	OnClose func(*Session)
}

// Inbound is an application frame delivered to the owner. Payload is a copy.
type Inbound struct {
	From    *Session
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

	ctx      context.Context
	cancel   context.CancelFunc
	live     atomic.Int32 // supervised goroutines still running
	connOnce sync.Once

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

	control    chan ctrl
	chat       chan outFrame
	inbound    chan<- Inbound // where frames go (own or shared)
	own        chan Inbound   // the session's own channel when none was supplied
	attached   chan struct{}  // closed once the owner is ready to receive
	attachOnce sync.Once

	streams      *streamTable
	transferWake chan struct{}
	closedQueues map[uint16]chan outFrame // frames still to send on streams already closed
	rrPos        int                      // writer round-robin position

	peerHello wire.Hello
	helloRecv chan struct{} // closed when the peer's HELLO validated
	readyCh   chan struct{} // closed when both HELLOs are done (ours written, theirs validated)
	ready     atomic.Bool

	closed  chan struct{}
	errMu   sync.Mutex
	err     error
	byeSent atomic.Bool

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
		root:         cfg.Root,
		pending:      make(chan *pendingRekey, 1),
		control:      make(chan ctrl, wire.ControlQueueCap),
		chat:         make(chan outFrame, wire.ChatQueueCap),
		attached:     make(chan struct{}),
		streams:      newStreamTable(cfg.Initiator),
		transferWake: make(chan struct{}, 1),
		closedQueues: map[uint16]chan outFrame{},
		helloRecv:    make(chan struct{}),
		readyCh:      make(chan struct{}),
		closed:       make(chan struct{}),
	}
	if cfg.Inbound != nil {
		s.inbound = cfg.Inbound
	} else {
		s.own = make(chan Inbound, wire.ChatQueueCap)
		s.inbound = s.own
		close(s.attached) // standalone use: deliver right away
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

// Start launches the reader, writer and controller: exactly three goroutines
// per session. It returns immediately; Ready() closes when both HELLOs
// validated, Done() when the session ends.
func (s *Session) Start(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.live.Store(3)
	s.spawn("reader", s.readLoop)
	s.spawn("writer", s.writeLoop)
	s.spawn("controller", s.controlLoop)
}

// spawn is the supervisor: it records the goroutine, converts a panic into a
// redacted log line, and tears the session down when goroutines exit. The
// first exit cancels the context and closes the connection so the others
// unwind; the last one clears the secrets.
func (s *Session) spawn(name string, fn func()) {
	go func() {
		defer s.exit()
		defer func() {
			if r := recover(); r != nil {
				if testing.Testing() && !s.cfg.RecoverInTests {
					panic(r) // a panic is always a bug: in tests it must fail the test (§12)
				}
				s.log.Error("session goroutine panic", "goroutine", name, "stack", redactStack(debug.Stack()))
				s.fail(fmt.Errorf("session: panic in %s", name))
			}
		}()
		fn()
	}()
}

// exit runs when a supervised goroutine returns.
func (s *Session) exit() {
	s.cancel()
	s.connOnce.Do(func() { _ = s.conn.Close() })
	if s.live.Add(-1) == 0 {
		s.finalize()
	}
}

// redactStack keeps function names and file:line pairs and drops argument
// words and offsets, so a logged stack can never carry memory contents.
func redactStack(b []byte) string {
	var sb strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "\t"): // "\t/path/file.go:123 +0x1f"
			if i := strings.LastIndex(line, " +0x"); i > 0 {
				line = line[:i]
			}
		case strings.HasPrefix(line, "goroutine "), line == "":
		default: // "pkg.Func(0xc000..., 0x1)" → "pkg.Func(...)"
			if i := strings.LastIndex(line, "("); i > 0 {
				line = line[:i] + "(...)"
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
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

// finalize clears every secret still in flight and reports the end of the session.
func (s *Session) finalize() {
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
	if s.cfg.OnClose != nil {
		s.cfg.OnClose(s)
	}
}

// Attach tells the session that its owner is ready for frames. Until then the
// reader holds them back (the peer feels it as TCP backpressure), so nothing
// is delivered before the owner knows the peer.
func (s *Session) Attach() { s.attachOnce.Do(func() { close(s.attached) }) }

// deliver hands one frame to the owner once attached.
func (s *Session) deliver(msg Inbound) error {
	msg.From = s
	select {
	case <-s.attached:
	case <-s.ctx.Done():
		return ErrClosed
	}
	select {
	case s.inbound <- msg:
		return nil
	case <-s.ctx.Done():
		return ErrClosed
	}
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

// Inbound delivers application frames when no shared channel was configured
// (nil otherwise).
func (s *Session) Inbound() <-chan Inbound { return s.own }

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

// TrySend queues a chat frame without blocking. It reports false when the
// chat queue is full; the caller keeps the frame and tries again later.
func (s *Session) TrySend(typ wire.FrameType, payload []byte) (bool, error) {
	if typ.Class() != wire.ClassChat {
		return false, wire.ErrStream
	}
	if !s.ready.Load() {
		return false, ErrNotReady
	}
	select {
	case <-s.closed:
		return false, ErrClosed
	default:
	}
	select {
	case s.chat <- outFrame{typ: typ, stream: wire.StreamChat, payload: append([]byte(nil), payload...)}:
		return true, nil
	default:
		return false, nil
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
