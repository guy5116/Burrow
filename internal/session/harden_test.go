package session

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("empty config accepted")
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	if _, err := New(Config{Conn: c1}); err == nil {
		t.Fatal("config without root accepted")
	}
}

func TestErrorsAndAccessors(t *testing.T) {
	e := &ByeError{Reason: 3}
	if !strings.Contains(e.Error(), "reason=3") || !errors.Is(e, ErrPeerBye) || errors.Is(e, ErrClosed) {
		t.Fatal(e)
	}
	p := newPair(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A cancelled context unblocks Send and SendStream when queues are full.
	for len(p.i.chat) < cap(p.i.chat) {
		p.i.chat <- outFrame{typ: wire.TypeTyping, stream: 1, payload: []byte{0}}
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < cap(p.i.chat)+4; i++ {
		if err := p.i.Send(ctx, wire.TypeTyping, []byte{0}); errors.Is(err, context.Canceled) {
			goto streams
		}
	}
	t.Fatal("Send never reported the cancelled context")
streams:
	id, _ := p.i.OpenStream(10)
	if err := p.i.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10)); err != nil {
		t.Fatal(err)
	}
	if err := p.i.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10)); err != nil {
		t.Log("second offer refused locally:", err) // state is still Offered; both outcomes are legal locally
	}
	if err := p.i.SendStream(context.Background(), id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); !errors.Is(err, ErrStreamState) {
		t.Fatal("accept on our own offer", err)
	}
	if err := p.i.SendStream(context.Background(), id, wire.TypeImgDone, nil); !errors.Is(err, ErrStreamState) {
		t.Fatal("done before accept", err)
	}
}

func TestNotReady(t *testing.T) {
	id, _ := identity.Generate()
	root, _ := secret.Random(32)
	c1, c2 := net.Pipe()
	defer c2.Close()
	s, err := New(Config{Conn: c1, Root: root, Initiator: false, Self: id})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	if err := s.Send(ctx, wire.TypeText, nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := s.SendStream(ctx, 2, wire.TypeImgOffer, nil); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
	if err := s.SendStream(ctx, 2, wire.TypeText, nil); !errors.Is(err, wire.ErrStream) {
		t.Fatal(err)
	}
	s.Close(wire.ByeUserQuit) // not ready: no BYE, just closes
	cancel()
	<-s.Done()
	if !errors.Is(s.Err(), ErrClosed) {
		t.Fatal(s.Err())
	}
}

// The controller loop itself (tests elsewhere call tick directly).
func TestControllerLoopSendsPingAndEnforcesFailSafe(t *testing.T) {
	p := newPair(t, 5*time.Millisecond)
	p.i.lastWrittenAt.Store(time.Now().Add(-wire.PingInterval - time.Second).UnixNano())
	deadline := time.Now().Add(3 * time.Second)
	for p.i.pongNonce.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("controller never pinged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.r.requestSentAt.Store(time.Now().Add(-wire.RekeyRequestTimeout - time.Second).UnixNano())
	select {
	case <-p.r.Done():
		if !errors.Is(p.r.Err(), ErrProtocol) {
			t.Fatal(p.r.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fail-safe did not close the session")
	}
}

func TestSupervisorRecoversPanic(t *testing.T) {
	p := newPair(t, time.Hour)
	p.i.spawn("boom", func() { panic("bug") })
	select {
	case <-p.i.Done():
		if p.i.Err() == nil || !strings.Contains(p.i.Err().Error(), "panic in boom") {
			t.Fatal(p.i.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic did not tear the session down")
	}
	// The peer's session ends too; the process (this test) is alive.
	<-p.r.Done()
}

func TestPendingFullIsABug(t *testing.T) {
	p := newPair(t, time.Hour)
	esc, _ := secret.Random(32)
	seed, _ := secret.Random(64)
	p.i.pending <- &pendingRekey{escalar: esc, kemSeed: seed}
	_ = p.i.Rekey()
	select {
	case <-p.i.Done():
		if !errors.Is(p.i.Err(), ErrProtocol) {
			t.Fatal(p.i.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("full pending channel did not close the session")
	}
}

func TestPushControlCtx(t *testing.T) {
	p := newPair(t, time.Hour)
	// Stall the writer: the conn blocks because the peer's reader is gone.
	p.cancel()
	<-p.i.Done()
	s := &Session{control: make(chan ctrl, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.control <- ctrl{}
	ck, _ := secret.Random(32)
	s.cancel()
	if err := s.pushControlCtx(ctrl{kind: ctrlRekeyDone, chain: ck}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if ck.Bytes()[0] != 0 && ck.Bytes()[1] != 0 {
		// cleared buffers read as zero
		t.Fatal("chain key not cleared on failure")
	}
	if testing.Short() {
		return
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	defer s.cancel()
	start := time.Now()
	if err := s.pushControlCtx(ctrl{kind: ctrlPong}); !errors.Is(err, ErrProtocol) || time.Since(start) < wire.ControlQueueStall {
		t.Fatal(err, time.Since(start))
	}
}

func TestWriterStopsOnBrokenConn(t *testing.T) {
	p := newPair(t, time.Hour)
	_ = p.i.conn.Close()
	sendErr := p.i.Send(context.Background(), wire.TypeTyping, []byte{1})
	select {
	case <-p.i.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("session survived a closed connection", sendErr)
	}
	if p.i.Err() == nil {
		t.Fatal("no error recorded")
	}
	if err := p.i.Rekey(); err != nil && !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
}

func TestResponderRekeyHelpers(t *testing.T) {
	p := newPair(t, time.Hour)
	// StartRekey sent to a responder's writer is ignored; SendRekeyRequest to an initiator too.
	if err := p.r.pushControl(ctrl{kind: ctrlStartRekey}); err != nil {
		t.Fatal(err)
	}
	if err := p.i.pushControl(ctrl{kind: ctrlSendRekeyRequest}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if se, re := p.i.Epochs(); se != 0 || re != 0 || p.i.Err() != nil || p.r.Err() != nil {
		t.Fatal("ignored commands had an effect")
	}
	if trimWindow(nil, time.Now(), time.Second) != nil && len(trimWindow(nil, time.Now(), time.Second)) != 0 {
		t.Fatal("trimWindow")
	}
	now := time.Now()
	ts := []time.Time{now.Add(-3 * time.Second), now.Add(-time.Second / 2), now}
	if got := trimWindow(ts, now, time.Second); len(got) != 2 {
		t.Fatal(len(got))
	}
	for i := 0; i < 50; i++ {
		if n, err := randomNonce(); err != nil || n == 0 {
			t.Fatal(n, err)
		}
	}
}

func TestSendStreamStateErrors(t *testing.T) {
	a := newAdv(t, false, true) // session is responder; the raw peer offers even ids
	ctx := context.Background()
	// Accept limit: two active incoming transfers, the third accept is refused locally.
	for i := 0; i < wire.MaxActiveTransfers+1; i++ {
		id := uint16(2 + 2*i)
		if err := a.p.SendFrame(wire.TypeImgOffer, id, offerPayload(3*wire.ChunkData)); err != nil {
			t.Fatal(err)
		}
		a.recvType(t, wire.TypeImgOffer)
		err := a.s.SendStream(ctx, id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0))
		if i < wire.MaxActiveTransfers {
			if err != nil {
				t.Fatal(err)
			}
			a.recv(t, wire.TypeImgAccept)
		} else if !errors.Is(err, ErrStreamLimit) {
			t.Fatal("third active transfer accepted", err)
		}
	}
	if err := a.s.SendStream(ctx, 2, wire.TypeImgAccept, []byte{1}); !errors.Is(err, ErrStreamState) {
		t.Fatal("second accept", err)
	}
	if err := a.s.SendStream(ctx, 6, wire.TypeImgAccept, []byte{1, 2}); err == nil {
		t.Fatal("malformed accept payload")
	}
	// Once draining, nothing but waiting is allowed on the stream.
	if err := a.s.SendStream(ctx, 2, wire.TypeImgCancel, wire.AppendImgCancel(nil, 0)); err != nil {
		t.Fatal(err)
	}
	a.recv(t, wire.TypeImgCancel)
	for _, typ := range []wire.FrameType{wire.TypeImgCancel, wire.TypeImgResult, wire.TypeImgReject} {
		if err := a.s.SendStream(ctx, 2, typ, []byte{0}); !errors.Is(err, ErrStreamState) {
			t.Fatalf("%#x while draining: %v", typ, err)
		}
	}
	// Frames from the peer on a draining stream are discarded, not violations.
	c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: make([]byte, wire.ChunkData)})
	if err := a.p.SendFrame(wire.TypeImgChunk, 2, c); err != nil {
		t.Fatal(err)
	}
	if err := a.p.SendFrame(wire.TypeImgCancel, 2, wire.AppendImgCancel(nil, 0)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for a.s.StreamOpen(2) {
		if time.Now().After(deadline) {
			t.Fatal("draining stream did not close on the peer's terminal frame")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if a.s.Err() != nil {
		t.Fatal(a.s.Err())
	}
}

func TestStreamPayloadViolations(t *testing.T) {
	cases := map[string]func(a *adv){
		"reject bad reason": func(a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgReject, id, []byte{9})
		},
		"reject after accept": func(a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgAccept, id, wire.AppendImgAccept(nil, 0))
			a.recvType(t, wire.TypeImgAccept)
			_ = a.p.SendFrame(wire.TypeImgReject, id, []byte{0})
		},
		"result bad status": func(a *adv) {
			id, _ := a.s.OpenStream(1)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(1))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgAccept, id, wire.AppendImgAccept(nil, 0))
			a.recvType(t, wire.TypeImgAccept)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: []byte{1}})
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgChunk, c)
			a.recv(t, wire.TypeImgChunk)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgDone, nil)
			a.recv(t, wire.TypeImgDone)
			_ = a.p.SendFrame(wire.TypeImgResult, id, []byte{7})
		},
		"accept malformed": func(a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgAccept, id, []byte{1})
		},
		"offer malformed": func(a *adv) { _ = a.p.SendFrame(wire.TypeImgOffer, 3, []byte{1, 2, 3}) },
		"chunk from the sender side": func(a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgAccept, id, wire.AppendImgAccept(nil, 0))
			a.recvType(t, wire.TypeImgAccept)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: []byte{1}})
			_ = a.p.SendFrame(wire.TypeImgChunk, id, c)
		},
		"rekey_resp malformed": func(a *adv) {
			_ = a.s.Rekey()
			a.recv(t, wire.TypeRekeyInit)
			_ = a.p.SendFrame(wire.TypeRekeyResp, 0, []byte{1, 2})
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAdv(t, true, true) // session initiator: its own streams are even, the peer's odd
			run(a)
			a.expectViolation(t)
		})
	}
}

func TestRedactStack(t *testing.T) {
	in := "goroutine 7 [running]:\nmain.secretFunc(0xc000123456, 0x41414141, {0xc0000a, 0x20, 0x20})\n\t/src/x/main.go:42 +0x1f\ncreated by main.start in goroutine 1\n\t/src/x/main.go:10 +0x5\n"
	out := redactStack([]byte(in))
	for _, leak := range []string{"0xc000123456", "0x41414141", "+0x1f", "0x20"} {
		if strings.Contains(out, leak) {
			t.Fatalf("stack still contains %s:\n%s", leak, out)
		}
	}
	for _, keep := range []string{"main.secretFunc(...)", "/src/x/main.go:42", "goroutine 7 [running]:"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("stack lost %q:\n%s", keep, out)
		}
	}
}
