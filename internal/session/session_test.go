package session

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type pairT struct {
	i, r   *Session
	cancel context.CancelFunc
}

// newPair builds two sessions sharing a random root over net.Pipe and waits
// for the HELLO exchange.
// testHello is what a real engine advertises: images, files and typing.
func testHello(name string) wire.Hello {
	return wire.Hello{Features: wire.FeatureTyping | wire.FeatureImages | wire.FeatureFiles, MaxImage: 1 << 30, MaxFile: 1 << 30, Name: []byte(name)}
}

func newPair(t *testing.T, tick time.Duration) *pairT {
	t.Helper()
	idI, _ := identity.Generate()
	idR, _ := identity.Generate()
	root, _ := secret.Random(wire.RootKeySize)
	rootCopy := secret.From(append([]byte(nil), root.Bytes()...))
	ci, cr := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	mk := func(conn net.Conn, root *secret.Buffer, init bool, self *identity.Identity, peer identity.PeerID, name string) *Session {
		s, err := New(Config{Conn: conn, Root: root, Initiator: init, Self: self, Peer: peer,
			Hello: testHello(name), Logger: slog.Default(), Tick: tick})
		if err != nil {
			t.Fatal(err)
		}
		s.Start(ctx)
		return s
	}
	p := &pairT{cancel: cancel}
	p.i = mk(ci, root, true, idI, idR.Public(), "init")
	p.r = mk(cr, rootCopy, false, idR, idI.Public(), "resp")
	for _, s := range []*Session{p.i, p.r} {
		select {
		case <-s.Ready():
		case <-s.Done():
			t.Fatalf("session died before ready: %v", s.Err())
		case <-time.After(5 * time.Second):
			t.Fatal("HELLO exchange timed out")
		}
	}
	t.Cleanup(func() {
		cancel()
		<-p.i.Done()
		<-p.r.Done()
	})
	return p
}

func recvText(t *testing.T, s *Session, want string) wire.Text {
	t.Helper()
	select {
	case in := <-s.Inbound():
		if in.Type != wire.TypeText {
			t.Fatalf("got type %#x", in.Type)
		}
		tx, err := wire.DecodeText(in.Payload)
		if err != nil || string(tx.Text) != want {
			t.Fatalf("%q %v", tx.Text, err)
		}
		return tx
	case <-s.Done():
		t.Fatalf("session closed: %v", s.Err())
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for text")
	}
	return wire.Text{}
}

func sendText(t *testing.T, s *Session, id uint64, text string) {
	t.Helper()
	p, _ := wire.AppendText(nil, wire.Text{MsgID: id, SentAt: 1, Text: []byte(text)})
	if err := s.Send(context.Background(), wire.TypeText, p); err != nil {
		t.Fatal(err)
	}
}

func TestHelloAndText(t *testing.T) {
	p := newPair(t, time.Hour)
	if string(p.i.PeerHello().Name) != "resp" || string(p.r.PeerHello().Name) != "init" {
		t.Fatal("peer hello")
	}
	if h := p.i.PeerHello(); h.Features != testHello("").Features || h.MaxFile != 1<<30 {
		t.Fatal("features")
	}
	sendText(t, p.i, 1, "hello")
	sendText(t, p.r, 2, "hi back")
	if tx := recvText(t, p.r, "hello"); tx.MsgID != 1 {
		t.Fatal(tx)
	}
	recvText(t, p.i, "hi back")
	// ACK and TYPING
	if err := p.r.Send(context.Background(), wire.TypeAck, wire.AppendAck(nil, wire.Ack{MsgID: 1, Status: 1})); err != nil {
		t.Fatal(err)
	}
	if in := <-p.i.Inbound(); in.Type != wire.TypeAck {
		t.Fatal(in.Type)
	}
	if err := p.i.Send(context.Background(), wire.TypeTyping, wire.AppendTyping(nil, 1)); err != nil {
		t.Fatal(err)
	}
	if in := <-p.r.Inbound(); in.Type != wire.TypeTyping {
		t.Fatal(in.Type)
	}
	if err := p.i.Send(context.Background(), wire.TypeHello, nil); !errors.Is(err, wire.ErrStream) {
		t.Fatal(err)
	}
	if p.i.Peer() != p.r.cfg.Self.Public() || !p.i.Initiator() || p.r.Initiator() {
		t.Fatal("identity accessors")
	}
}

func TestPingPong(t *testing.T) {
	p := newPair(t, time.Hour)
	if err := p.i.pushControl(ctrl{kind: ctrlSendPing}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.i.pongNonce.Load() == 0 || p.i.pongNonce.Load() != p.i.pingNonce.Load() {
		if time.Now().After(deadline) {
			t.Fatal("no PONG")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The controller sends a PING after PingInterval of silence.
	p.i.lastWrittenAt.Store(time.Now().Add(-wire.PingInterval - time.Second).UnixNano())
	var fs time.Time
	if err := p.i.tick(time.Now(), &fs); err != nil {
		t.Fatal(err)
	}
	first := p.i.pongNonce.Load()
	deadline = time.Now().Add(3 * time.Second)
	for p.i.pongNonce.Load() == first {
		if time.Now().After(deadline) {
			t.Fatal("no second PONG")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Not while one is outstanding.
	p.i.pingNonce.Store(p.i.pongNonce.Load() + 1)
	p.i.lastWrittenAt.Store(time.Now().Add(-time.Hour).UnixNano())
	if err := p.i.tick(time.Now(), &fs); err != nil || len(p.i.control) != 0 {
		t.Fatal("PING sent while outstanding")
	}
}

func waitEpoch(t *testing.T, s *Session, send, recv uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		se, re := s.Epochs()
		if se == send && re == recv {
			return
		}
		select {
		case <-s.Done():
			t.Fatalf("closed: %v", s.Err())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("epochs %d/%d, want %d/%d", se, re, send, recv)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Three rekey epochs while chat traffic flows both ways throughout.
func TestRekeyEpochsUnderTraffic(t *testing.T) {
	p := newPair(t, time.Hour)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		n := uint64(100)
		for {
			select {
			case <-stop:
				return
			default:
			}
			sendText(t, p.i, n, "i")
			sendText(t, p.r, n, "r")
			recvText(t, p.r, "i")
			recvText(t, p.i, "r")
			n++
		}
	}()
	for epoch := uint32(1); epoch <= 3; epoch++ {
		if err := p.i.Rekey(); err != nil {
			t.Fatal(err)
		}
		waitEpoch(t, p.i, epoch, epoch)
		waitEpoch(t, p.r, epoch, epoch)
		if p.i.rekeyInFlight.Load() || p.r.readerRekeyState.Load() != rekeyIdle || len(p.i.pending) != 0 {
			t.Fatal("rekey state not idle")
		}
	}
	close(stop)
	<-done
	if bytes.Equal(p.i.root.Bytes(), p.r.root.Bytes()) == false {
		t.Fatal("roots diverged")
	}
	// Chat still works.
	sendText(t, p.i, 9, "after")
	recvText(t, p.r, "after")
}

func TestRekeyRequestAndSchedule(t *testing.T) {
	p := newPair(t, time.Hour)
	// Responder requests; initiator rekeys.
	if err := p.r.Rekey(); err != nil {
		t.Fatal(err)
	}
	waitEpoch(t, p.i, 1, 1)
	waitEpoch(t, p.r, 1, 1)
	if p.r.requestSentAt.Load() != 0 {
		t.Fatal("requestSentAt not cleared by RESP")
	}
	// Initiator's controller starts a rekey at the counter threshold.
	p.i.sendCounter.Store(wire.RekeyInitCounter)
	var fs time.Time
	if err := p.i.tick(time.Now(), &fs); err != nil {
		t.Fatal(err)
	}
	waitEpoch(t, p.i, 2, 2)
	waitEpoch(t, p.r, 2, 2)
	// Responder's controller requests at its threshold.
	p.r.recvCounter.Store(wire.RekeyRequestCounter)
	if err := p.r.tick(time.Now(), &fs); err != nil {
		t.Fatal(err)
	}
	waitEpoch(t, p.i, 3, 3)
	waitEpoch(t, p.r, 3, 3)
	// Time-based: 15 minutes since the switch.
	p.i.sendSwitchedAt.Store(time.Now().Add(-wire.RekeyInitInterval).UnixNano())
	if err := p.i.tick(time.Now(), &fs); err != nil {
		t.Fatal(err)
	}
	waitEpoch(t, p.i, 4, 4)
	waitEpoch(t, p.r, 4, 4)
}

// A StartRekey landing between RESP and DONE (rekeyInFlight still set) is discarded.
func TestStartRekeyWhileInFlightDiscarded(t *testing.T) {
	p := newPair(t, time.Hour)
	// Hold the responder's writer so RESP is delayed: fill nothing, just race two StartRekeys.
	if err := p.i.Rekey(); err != nil {
		t.Fatal(err)
	}
	if err := p.i.Rekey(); err != nil {
		t.Fatal(err)
	}
	waitEpoch(t, p.i, 1, 1)
	waitEpoch(t, p.r, 1, 1)
	time.Sleep(50 * time.Millisecond)
	if se, _ := p.i.Epochs(); se != 1 || p.i.rekeyInFlight.Load() || len(p.i.pending) != 0 {
		t.Fatal("second StartRekey was not discarded")
	}
	// Directly: the writer's guard.
	p.i.rekeyInFlight.Store(true)
	if err := p.i.Rekey(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if len(p.i.pending) != 0 {
		t.Fatal("rekey started while in flight")
	}
	p.i.rekeyInFlight.Store(false)
}

func TestByeAndClose(t *testing.T) {
	p := newPair(t, time.Hour)
	p.i.Close(wire.ByeUserQuit)
	select {
	case <-p.r.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("responder did not close on BYE")
	}
	var be *ByeError
	if !errors.As(p.r.Err(), &be) || be.Reason != wire.ByeUserQuit || !errors.Is(p.r.Err(), ErrPeerBye) {
		t.Fatal(p.r.Err())
	}
	if !errors.Is(p.i.Err(), ErrClosed) {
		t.Fatal(p.i.Err())
	}
	if err := p.i.Send(context.Background(), wire.TypeText, nil); err == nil {
		t.Fatal("send after close")
	}
	// Secrets cleared.
	if !bytes.Equal(p.i.root.Bytes(), make([]byte, 32)) || !bytes.Equal(p.i.send.ck.Bytes(), make([]byte, 32)) {
		t.Fatal("secrets not cleared")
	}
	p.i.Close(0) // idempotent
}

func TestFailSafes(t *testing.T) {
	p := newPair(t, time.Hour)
	var fs time.Time
	now := time.Now()
	// rekeyInFlight for > 30 s
	p.i.rekeyInFlight.Store(true)
	p.i.rekeyStartedAt.Store(now.Add(-wire.RekeyInFlightTimeout - time.Second).UnixNano())
	if err := p.i.tick(now, &fs); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	p.i.rekeyInFlight.Store(false)
	// 25 min since switch
	p.i.recvSwitchedAt.Store(now.Add(-wire.RekeyEpochFailSafe - time.Second).UnixNano())
	if err := p.i.tick(now, &fs); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	p.i.recvSwitchedAt.Store(now.UnixNano())
	// REQUEST unanswered for 10 s
	p.r.requestSentAt.Store(now.Add(-wire.RekeyRequestTimeout - time.Second).UnixNano())
	if err := p.r.tick(now, &fs); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	p.r.requestSentAt.Store(0)
	// AwaitingDone for > 30 s
	p.r.readerRekeyState.Store(rekeyAwaitingDone)
	p.r.readerStateSince.Store(now.Add(-wire.RekeyInFlightTimeout - time.Second).UnixNano())
	if err := p.r.tick(now, &fs); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	p.r.readerRekeyState.Store(rekeyIdle)
	// Control queue full for 5 s → stalled writer.
	for len(p.i.control) < cap(p.i.control) {
		p.i.control <- ctrl{kind: ctrlKind(99)}
	}
	p.i.sendCounter.Store(wire.RekeyInitCounter)
	if err := p.i.tick(now, &fs); err != nil || fs.IsZero() {
		t.Fatal("full queue not noted", err)
	}
	if err := p.i.tick(now.Add(wire.ControlQueueStall), &fs); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

func TestEpochExhausted(t *testing.T) {
	ck, _ := secret.Random(32)
	c := newChain(ck, 0)
	c.counter = wire.EpochMaxFrames - 1
	if err := c.next(); err != nil {
		t.Fatal(err)
	}
	if err := c.next(); !errors.Is(err, ErrEpochExhausted) {
		t.Fatal(err)
	}
}

func TestChainKAT(t *testing.T) {
	// Chains are HMAC-SHA256(ck, 0x01) / (ck, 0x02); pin one step so both
	// directions and any reimplementation agree.
	ck := secret.From(bytes.Repeat([]byte{0x11}, 32))
	c := newChain(ck, 0)
	if err := c.next(); err != nil {
		t.Fatal(err)
	}
	if got := c.mk[:4]; !bytes.Equal(got, chainKATmk) {
		t.Fatalf("mk %x", c.mk)
	}
	if got := c.ck.Bytes()[:4]; !bytes.Equal(got, chainKATck) {
		t.Fatalf("ck' %x", c.ck.Bytes())
	}
	root := secret.From(bytes.Repeat([]byte{0x22}, 32))
	i2r, r2i, _ := deriveChains(root, 7)
	if bytes.Equal(i2r.Bytes(), r2i.Bytes()) || !bytes.Equal(i2r.Bytes()[:4], chainKATi2r) {
		t.Fatalf("i2r %x", i2r.Bytes())
	}
}

var (
	chainKATmk  = []byte{0xc4, 0xed, 0xe0, 0x64}
	chainKATck  = []byte{0xed, 0x50, 0xb2, 0x71}
	chainKATi2r = []byte{0xeb, 0x73, 0x82, 0x4a}
)

// raceEnabled is set by race_test.go; the race detector changes escape
// analysis, so allocation budgets are asserted without it (make test runs both).
var raceEnabled bool

func TestSealOpenAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation budget is measured without -race")
	}
	idI, _ := identity.Generate()
	root, _ := secret.Random(32)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	s, _ := New(Config{Conn: c1, Root: root, Initiator: true, Self: idI})
	defer s.send.clear()
	defer s.recv.clear()
	fb := make([]byte, wire.MaxOuterFrame)
	payload := make([]byte, 1000)
	recv := newChain(secret.From(append([]byte(nil), s.send.ck.Bytes()...)), 0)
	defer recv.clear()
	r := &Session{cfg: Config{Initiator: false}}
	const runs = 50
	frames := make([][]byte, runs+1)
	for i := range frames {
		frames[i] = make([]byte, wire.MaxOuterFrame)
	}
	j := 0
	sealAllocs := testing.AllocsPerRun(runs, func() {
		inner, _ := wire.EncodeInner(fb[4:4], wire.TypeText, wire.StreamChat, payload)
		frame, err := s.seal(s.send, fb, len(inner))
		if err != nil {
			t.Fatal(err)
		}
		frames[j] = frames[j][:copy(frames[j], frame)]
		j++
	})
	i := 0
	openAllocs := testing.AllocsPerRun(runs, func() {
		if _, err := r.open(recv, frames[i][4:]); err != nil {
			t.Fatal(err)
		}
		i++
	})
	if sealAllocs > 8 || openAllocs > 8 {
		t.Fatalf("seal allocs %v, open allocs %v (budget 8 each)", sealAllocs, openAllocs)
	}
}

func BenchmarkSealOpen(b *testing.B) {
	idI, _ := identity.Generate()
	root, _ := secret.Random(32)
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	s, _ := New(Config{Conn: c1, Root: root, Initiator: true, Self: idI})
	recv := newChain(secret.From(append([]byte(nil), s.send.ck.Bytes()...)), 0)
	r := &Session{cfg: Config{Initiator: false}}
	fb := make([]byte, wire.MaxOuterFrame)
	payload := make([]byte, wire.MaxPayload)
	b.SetBytes(int64(wire.MaxInner))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if i%wire.EpochMaxFrames == wire.EpochMaxFrames-1 {
			s.send.counter, recv.counter = 0, 0
		}
		inner, _ := wire.EncodeInner(fb[4:4], wire.TypeImgChunk, 2, payload)
		frame, err := s.seal(s.send, fb, len(inner))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.open(recv, frame[4:]); err != nil {
			b.Fatal(err)
		}
	}
}
