package session

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/wire"
)

// adv builds one real session (initiator or responder) against a raw test
// peer sharing the root. When hello is true both HELLOs are exchanged first.
type adv struct {
	s    *Session
	p    *testpeer.Peer
	self *identity.Identity
	peer *identity.Identity
}

func newAdv(t *testing.T, sessionIsInitiator, hello bool) *adv {
	t.Helper()
	self, _ := identity.Generate()
	other, _ := identity.Generate()
	root, _ := secret.Random(32)
	rootBytes := append([]byte(nil), root.Bytes()...)
	cs, cp := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(Config{Conn: cs, Root: root, Initiator: sessionIsInitiator, Self: self, Peer: other.Public(),
		Hello: wire.Hello{}, Logger: slog.Default(), Tick: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	p := testpeer.New(cp, rootBytes, !sessionIsInitiator, other.Scalar(), self.Public())
	t.Cleanup(func() {
		cancel()
		<-s.Done()
		_ = cp.Close()
	})
	a := &adv{s: s, p: p, self: self, peer: other}
	if hello {
		if sessionIsInitiator {
			a.recv(t, wire.TypeHello)
			if err := p.Hello(wire.Hello{}); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := p.Hello(wire.Hello{}); err != nil {
				t.Fatal(err)
			}
			a.recv(t, wire.TypeHello)
		}
		select {
		case <-s.Ready():
		case <-time.After(3 * time.Second):
			t.Fatal("not ready")
		}
	}
	return a
}

func (a *adv) recv(t *testing.T, typ wire.FrameType) wire.Inner {
	t.Helper()
	in, err := a.p.Recv(3 * time.Second)
	if err != nil {
		t.Fatalf("recv: %v (session err %v)", err, a.s.Err())
	}
	if in.Type != typ {
		t.Fatalf("got %#x want %#x", in.Type, typ)
	}
	return in
}

// expectViolation asserts the session closed with ErrProtocol and wrote nothing (no BYE).
func (a *adv) expectViolation(t *testing.T) {
	t.Helper()
	if err := a.p.ExpectClose(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	<-a.s.Done()
	if !errors.Is(a.s.Err(), ErrProtocol) {
		t.Fatalf("session error %v, want ErrProtocol", a.s.Err())
	}
}

func TestOuterLengthRejectedBeforeBody(t *testing.T) {
	for _, n := range []uint32{0, wire.MinCiphertext - 1, wire.MaxCiphertext + 1, 1 << 31} {
		a := newAdv(t, false, false)
		if err := a.p.SendOuter(n, nil); err != nil { // header only: no body ever follows
			t.Fatal(err)
		}
		a.expectViolation(t)
	}
}

func TestFlippedCiphertextBit(t *testing.T) {
	a := newAdv(t, false, false)
	b, _ := wire.AppendHello(nil, wire.Hello{})
	inner, _ := wire.EncodeInner(nil, wire.TypeHello, 0, b)
	frame := a.p.Seal(inner)
	frame[len(frame)-1] ^= 1
	_, _ = a.p.Conn.Write(frame)
	a.expectViolation(t)
}

func TestInnerViolations(t *testing.T) {
	hello, _ := wire.AppendHello(nil, wire.Hello{})
	good, _ := wire.EncodeInner(nil, wire.TypeHello, 0, hello)
	cases := map[string]func() []byte{
		"non-zero flags": func() []byte { b := append([]byte(nil), good...); b[1] = 1; return b },
		"bad padding":    func() []byte { return append(append([]byte(nil), good...), 0) },
		"short padding":  func() []byte { return good[:len(good)-1] },
		"unknown type":   func() []byte { b := append([]byte(nil), good...); b[0] = 0x7f; return b },
		"hello on chat":  func() []byte { b := append([]byte(nil), good...); b[3] = 1; return b },
		"text before hello": func() []byte {
			p, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("x")})
			b, _ := wire.EncodeInner(nil, wire.TypeText, 1, p)
			return b
		},
		"bye before hello": func() []byte { b, _ := wire.EncodeInner(nil, wire.TypeBye, 0, []byte{0}); return b },
		"ping before hello": func() []byte {
			b, _ := wire.EncodeInner(nil, wire.TypePing, 0, wire.AppendPing(nil, 1))
			return b
		},
		"reserved feature bit": func() []byte {
			h, _ := wire.AppendHello(nil, wire.Hello{Features: 1 << 5})
			b, _ := wire.EncodeInner(nil, wire.TypeHello, 0, h)
			return b
		},
		"max_image without images": func() []byte {
			h, _ := wire.AppendHello(nil, wire.Hello{MaxImage: 1})
			b, _ := wire.EncodeInner(nil, wire.TypeHello, 0, h)
			return b
		},
		"images without max_image": func() []byte {
			h, _ := wire.AppendHello(nil, wire.Hello{Features: wire.FeatureImages})
			b, _ := wire.EncodeInner(nil, wire.TypeHello, 0, h)
			return b
		},
		"hello name invalid utf8": func() []byte {
			h, _ := wire.AppendHello(nil, wire.Hello{Name: []byte{0xff}})
			b, _ := wire.EncodeInner(nil, wire.TypeHello, 0, h)
			return b
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAdv(t, false, false)
			_ = a.p.SendInner(mk()) // the session may close before the body is fully written
			a.expectViolation(t)
		})
	}
}

func TestPostHelloViolations(t *testing.T) {
	type tc struct {
		sessionInitiator bool
		typ              wire.FrameType
		stream           uint16
		payload          []byte
	}
	hello, _ := wire.AppendHello(nil, wire.Hello{})
	badText, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("ok")})
	badText = append(badText, 0xc0) // invalid UTF-8 tail
	var ri wire.RekeyInit
	var rr wire.RekeyResp
	cases := map[string]tc{
		"second hello":                 {false, wire.TypeHello, 0, hello},
		"pong without ping":            {false, wire.TypePong, 0, wire.AppendPong(nil, 5)},
		"rekey_init from responder":    {true, wire.TypeRekeyInit, 0, wire.AppendRekeyInit(nil, &ri)},
		"rekey_resp unsolicited":       {true, wire.TypeRekeyResp, 0, wire.AppendRekeyResp(nil, &rr)},
		"rekey_done when idle":         {false, wire.TypeRekeyDone, 0, nil},
		"rekey_request from initiator": {false, wire.TypeRekeyRequest, 0, nil},
		"rekey_init to initiator":      {true, wire.TypeRekeyInit, 0, wire.AppendRekeyInit(nil, &ri)},
		"rekey_resp to responder":      {false, wire.TypeRekeyResp, 0, wire.AppendRekeyResp(nil, &rr)},
		"rekey_done to initiator":      {true, wire.TypeRekeyDone, 0, nil},
		"invalid utf8 text":            {false, wire.TypeText, 1, badText},
		"ack bad status":               {false, wire.TypeAck, 1, wire.AppendAck(nil, wire.Ack{MsgID: 1, Status: 2})},
		"typing bad state":             {false, wire.TypeTyping, 1, []byte{2}},
		"bye bad reason":               {false, wire.TypeBye, 0, []byte{9}},
		"image frame before phase 2":   {false, wire.TypeImgOffer, 2, nil},
		"rekey_request with payload":   {true, wire.TypeRekeyRequest, 0, []byte{0}},
		"rekey_init malformed":         {false, wire.TypeRekeyInit, 0, []byte{1, 2, 3}},
		"rekey_init bad kem key":       {false, wire.TypeRekeyInit, 0, make([]byte, 32+1184)},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAdv(t, c.sessionInitiator, true)
			if err := a.p.SendFrame(c.typ, c.stream, c.payload); err != nil {
				t.Fatal(err)
			}
			a.expectViolation(t)
		})
	}
}

func TestRekeyBadKEMKeyClosesWithoutLeak(t *testing.T) {
	a := newAdv(t, false, true)
	var ri wire.RekeyInit
	for i := range ri.KemEK {
		ri.KemEK[i] = 0xff
	}
	if err := a.p.SendFrame(wire.TypeRekeyInit, 0, wire.AppendRekeyInit(nil, &ri)); err != nil {
		t.Fatal(err)
	}
	a.expectViolation(t)
}

func TestPongNonceRules(t *testing.T) {
	// wrong nonce
	a := newAdv(t, true, true)
	_ = a.s.pushControl(ctrl{kind: ctrlSendPing})
	in := a.recv(t, wire.TypePing)
	n, _ := wire.DecodePing(in.Payload)
	if err := a.p.SendFrame(wire.TypePong, 0, wire.AppendPong(nil, n^1)); err != nil {
		t.Fatal(err)
	}
	a.expectViolation(t)

	// duplicate PONG
	b := newAdv(t, true, true)
	_ = b.s.pushControl(ctrl{kind: ctrlSendPing})
	in = b.recv(t, wire.TypePing)
	n, _ = wire.DecodePing(in.Payload)
	if err := b.p.SendFrame(wire.TypePong, 0, wire.AppendPong(nil, n)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if b.s.pongNonce.Load() != n {
		t.Fatal("first PONG not accepted")
	}
	if err := b.p.SendFrame(wire.TypePong, 0, wire.AppendPong(nil, n)); err != nil {
		t.Fatal(err)
	}
	b.expectViolation(t)
}

func TestPingFloodAndTypingFlood(t *testing.T) {
	a := newAdv(t, false, true)
	for i := 0; i < wire.MaxPingsPerWindow; i++ {
		if err := a.p.SendFrame(wire.TypePing, 0, wire.AppendPing(nil, uint64(i+1))); err != nil {
			t.Fatal(err)
		}
		a.recv(t, wire.TypePong)
	}
	if err := a.p.SendFrame(wire.TypePing, 0, wire.AppendPing(nil, 99)); err != nil {
		t.Fatal(err)
	}
	a.expectViolation(t)

	b := newAdv(t, false, true)
	for i := 0; i < wire.MaxTypingPerWindow+5; i++ {
		if err := b.p.SendFrame(wire.TypeTyping, 1, wire.AppendTyping(nil, 1)); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	for {
		select {
		case <-b.s.Inbound():
			got++
			continue
		case <-time.After(100 * time.Millisecond):
		}
		break
	}
	if got != wire.MaxTypingPerWindow {
		t.Fatalf("delivered %d TYPING frames, want %d (rest dropped, not closed)", got, wire.MaxTypingPerWindow)
	}
	if b.s.Err() != nil {
		t.Fatal("session closed on typing flood")
	}
}

// A peer holding the session state but not the identity key cannot complete a
// rekey: the next epoch fails on both sides.
func TestRekeyWithoutIdentityKeyFails(t *testing.T) {
	for _, sessionInitiator := range []bool{true, false} {
		a := newAdv(t, sessionInitiator, true)
		wrong, _ := identity.Generate()
		a.p.Static = wrong.Scalar() // attacker's own key instead of the stolen identity
		if sessionInitiator {
			_ = a.s.Rekey()
			in := a.recv(t, wire.TypeRekeyInit)
			if err := a.p.AnswerRekey(in.Payload); err != nil {
				t.Fatal(err)
			}
			// Session (initiator) derives a different root; its DONE is under epoch 0 (readable),
			// but its epoch-1 frames are not, and our epoch-1 frames fail on its side.
			a.recv(t, wire.TypeRekeyDone)
			a.p.AcceptDone()
		} else {
			if err := a.p.StartRekey(); err != nil {
				t.Fatal(err)
			}
			in := a.recv(t, wire.TypeRekeyResp)
			if err := a.p.FinishRekey(in.Payload); err != nil {
				t.Fatal(err)
			}
		}
		txt, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("evicted?")})
		if err := a.p.SendFrame(wire.TypeText, 1, txt); err != nil {
			t.Fatal(err)
		}
		a.expectViolation(t)
	}
}

// The honest rekey path through the independent test-peer implementation
// (interop check of the derivation).
func TestRekeyInteropWithTestPeer(t *testing.T) {
	for _, sessionInitiator := range []bool{true, false} {
		a := newAdv(t, sessionInitiator, true)
		if sessionInitiator {
			_ = a.s.Rekey()
			in := a.recv(t, wire.TypeRekeyInit)
			if err := a.p.AnswerRekey(in.Payload); err != nil {
				t.Fatal(err)
			}
			a.recv(t, wire.TypeRekeyDone)
			a.p.AcceptDone()
		} else {
			if err := a.p.StartRekey(); err != nil {
				t.Fatal(err)
			}
			in := a.recv(t, wire.TypeRekeyResp)
			if err := a.p.FinishRekey(in.Payload); err != nil {
				t.Fatal(err)
			}
		}
		txt, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("epoch1")})
		if err := a.p.SendFrame(wire.TypeText, 1, txt); err != nil {
			t.Fatal(err)
		}
		recvText(t, a.s, "epoch1")
		sendText(t, a.s, 2, "back")
		in := a.recv(t, wire.TypeText)
		if tx, _ := wire.DecodeText(in.Payload); string(tx.Text) != "back" {
			t.Fatal("text after rekey")
		}
		if a.p.SendChain.Epoch != 1 || a.p.RecvChain.Epoch != 1 {
			t.Fatal("test peer epochs")
		}
	}
}

// 65,537th frame in one epoch: refused synchronously by the writer on send and
// the reader on receipt.
func TestEpochCounterCap(t *testing.T) {
	a := newAdv(t, true, true)
	a.p.RecvChain.Counter = wire.EpochMaxFrames // pretend we already read the max
	a.s.send.counter = wire.EpochMaxFrames
	sendErr := a.s.Send(context.Background(), wire.TypeText, func() []byte { p, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("x")}); return p }())
	if sendErr != nil {
		t.Fatal(sendErr)
	}
	<-a.s.Done()
	if !errors.Is(a.s.Err(), ErrEpochExhausted) {
		t.Fatal(a.s.Err())
	}

	b := newAdv(t, true, true)
	b.s.recv.counter = wire.EpochMaxFrames
	b.p.SendChain.Counter = wire.EpochMaxFrames
	txt, _ := wire.AppendText(nil, wire.Text{MsgID: 1, Text: []byte("x")})
	if err := b.p.SendFrame(wire.TypeText, 1, txt); err != nil {
		t.Fatal(err)
	}
	<-b.s.Done()
	if !errors.Is(b.s.Err(), ErrEpochExhausted) {
		t.Fatal(b.s.Err())
	}
}

func TestDeadPeerReadDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits DeadPeerTimeout")
	}
	a := newAdv(t, true, true)
	start := time.Now()
	<-a.s.Done()
	if d := time.Since(start); d < wire.DeadPeerTimeout-time.Second || d > wire.DeadPeerTimeout+5*time.Second {
		t.Fatal(d)
	}
}
