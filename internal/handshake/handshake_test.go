package handshake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flynn/noise"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/wire"
)

func newID(t *testing.T) *identity.Identity {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// countConn counts bytes written and records everything read.
type countConn struct {
	net.Conn
	written atomic.Int64
	seen    bytes.Buffer
}

func (c *countConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.written.Add(int64(n))
	return n, err
}

func (c *countConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.seen.Write(b[:n])
	return n, err
}

type outcome struct {
	res *Result
	err error
}

func run(t *testing.T, ctx context.Context, i, r *identity.Identity, peer identity.PeerID, token *[16]byte, auth Authorizer, replay *ReplayLRU) (outcome, outcome, *countConn) {
	t.Helper()
	ci, cr := net.Pipe()
	rc := &countConn{Conn: cr}
	ich, rch := make(chan outcome, 1), make(chan outcome, 1)
	go func() {
		res, err := Initiate(ctx, ci, i, peer, token)
		ich <- outcome{res, err}
		_ = ci.Close()
	}()
	go func() {
		res, err := Respond(ctx, rc, r, auth, replay)
		rch <- outcome{res, err}
		_ = cr.Close()
	}()
	return <-ich, <-rch, rc
}

func acceptAll(identity.PeerID, *[16]byte) Decision { return Accept }

func TestHandshakeSuccess(t *testing.T) {
	i, r := newID(t), newID(t)
	var gotPeer identity.PeerID
	var gotToken *[16]byte
	auth := func(p identity.PeerID, tok *[16]byte) Decision { gotPeer, gotToken = p, tok; return Accept }
	io_, ro, _ := run(t, context.Background(), i, r, r.Public(), nil, auth, NewReplayLRU())
	if io_.err != nil || ro.err != nil {
		t.Fatal(io_.err, ro.err)
	}
	if !bytes.Equal(io_.res.Root.Bytes(), ro.res.Root.Bytes()) || io_.res.Root.Len() != wire.RootKeySize {
		t.Fatal("roots differ")
	}
	if io_.res.Peer != r.Public() || ro.res.Peer != i.Public() || gotPeer != i.Public() {
		t.Fatal("peer ids")
	}
	if !io_.res.Initiator || ro.res.Initiator || gotToken != nil || ro.res.Token != nil {
		t.Fatal("flags")
	}
	// A second handshake yields a different root (fresh ephemerals).
	io2, _, _ := run(t, context.Background(), i, r, r.Public(), nil, auth, NewReplayLRU())
	if bytes.Equal(io2.res.Root.Bytes(), io_.res.Root.Bytes()) {
		t.Fatal("root reused")
	}
}

func TestHandshakeToken(t *testing.T) {
	i, r := newID(t), newID(t)
	tok := [16]byte{1, 2, 3}
	var got *[16]byte
	auth := func(_ identity.PeerID, t *[16]byte) Decision { got = t; return Accept }
	_, ro, _ := run(t, context.Background(), i, r, r.Public(), &tok, auth, nil)
	if ro.err != nil || got == nil || *got != tok || ro.res.Token == nil || *ro.res.Token != tok {
		t.Fatal(ro.err, got)
	}
}

func TestWrongResponderKey(t *testing.T) {
	i, r, other := newID(t), newID(t), newID(t)
	io_, ro, rc := run(t, context.Background(), i, r, other.Public(), nil, acceptAll, nil)
	var he *Error
	if !errors.As(ro.err, &he) || he.Stage != "msg1" {
		t.Fatalf("responder: %v", ro.err)
	}
	if rc.written.Load() != 0 {
		t.Fatal("responder wrote bytes on failure")
	}
	if !errors.As(io_.err, &he) || he.Stage != "msg2" {
		t.Fatalf("initiator: %v", io_.err)
	}
}

func TestUnauthorizedInitiator(t *testing.T) {
	i, r := newID(t), newID(t)
	reject := func(identity.PeerID, *[16]byte) Decision { return Reject }
	io_, ro, _ := run(t, context.Background(), i, r, r.Public(), nil, reject, nil)
	if !errors.Is(ro.err, ErrRejected) {
		t.Fatal(ro.err)
	}
	// The initiator cannot tell; it learns at HELLO time (connection closed).
	if io_.err != nil {
		t.Fatal(io_.err)
	}
	// nil authorizer rejects everything
	_, ro, _ = run(t, context.Background(), i, r, r.Public(), nil, nil, nil)
	if !errors.Is(ro.err, ErrRejected) {
		t.Fatal(ro.err)
	}
}

func TestReplayedMsg1(t *testing.T) {
	i, r := newID(t), newID(t)
	lru := NewReplayLRU()
	_, ro, rc := run(t, context.Background(), i, r, r.Public(), nil, acceptAll, lru)
	if ro.err != nil {
		t.Fatal(ro.err)
	}
	msg1 := rc.seen.Bytes()[:wire.HSLenPrefix+wire.HS1Len]
	// Replay to a fresh responder sharing the LRU: rejected before any DH, nothing written.
	ci, cr := net.Pipe()
	rc2 := &countConn{Conn: cr}
	done := make(chan error, 1)
	go func() { _, err := Respond(context.Background(), rc2, r, acceptAll, lru); done <- err; _ = cr.Close() }()
	if _, err := ci.Write(msg1); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrReplay) {
		t.Fatal(err)
	}
	if rc2.written.Load() != 0 {
		t.Fatal("wrote on replay")
	}
}

// sendRaw feeds bytes to a responder and returns its error and bytes written.
func sendRaw(t *testing.T, r *identity.Identity, raw []byte) (int64, error) {
	t.Helper()
	ci, cr := net.Pipe()
	rc := &countConn{Conn: cr}
	done := make(chan error, 1)
	go func() { _, err := Respond(context.Background(), rc, r, acceptAll, nil); done <- err; _ = cr.Close() }()
	_, _ = ci.Write(raw)
	err := <-done
	_ = ci.Close()
	return rc.written.Load(), err
}

func TestBadMsg1(t *testing.T) {
	r := newID(t)
	// wrong length prefix
	hdr := []byte{0x04, 0xcf} // 1231
	n, err := sendRaw(t, r, append(hdr, make([]byte, 1231)...))
	if !errors.Is(err, ErrLength) || n != 0 {
		t.Fatal(err, n)
	}
	n, err = sendRaw(t, r, []byte{0xff, 0xff})
	if !errors.Is(err, ErrLength) || n != 0 {
		t.Fatal(err, n)
	}
	// right length, garbage: AEAD failure, nothing written
	raw := make([]byte, wire.HSLenPrefix+wire.HS1Len)
	binary.BigEndian.PutUint16(raw, wire.HS1Len)
	_, _ = rand.Read(raw[2:])
	n, err = sendRaw(t, r, raw)
	var he *Error
	if !errors.As(err, &he) || he.Stage != "msg1" || errors.Is(err, ErrLength) || n != 0 {
		t.Fatal(err, n)
	}
}

func TestInvalidKEMKey(t *testing.T) {
	i, r := newID(t), newID(t)
	ipub, rpub := i.Public(), r.Public()
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK, Initiator: true,
		Random: rand.Reader, Prologue: []byte(wire.Prologue),
		StaticKeypair: noise.DHKey{Private: i.Scalar(), Public: ipub[:]}, PeerStatic: rpub[:]})
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Repeat([]byte{0xff}, wire.HS1PayloadLen) // coefficients out of range
	msg1, _, _, err := hs.WriteMessage(make([]byte, 2, 2+wire.HS1Len), bad)
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(msg1, wire.HS1Len)
	n, err := sendRaw(t, r, msg1)
	if !errors.Is(err, ErrBadKEM) || n != 0 {
		t.Fatal(err, n)
	}
}

func TestBadMsg2AndMsg3(t *testing.T) {
	i, r := newID(t), newID(t)
	// Initiator receives a wrong-length msg2.
	ci, cr := net.Pipe()
	done := make(chan error, 1)
	go func() { _, err := Initiate(context.Background(), ci, i, r.Public(), nil); done <- err; _ = ci.Close() }()
	if _, err := io.ReadFull(cr, make([]byte, wire.HSLenPrefix+wire.HS1Len)); err != nil {
		t.Fatal(err)
	}
	_, _ = cr.Write([]byte{0x04, 0x00})
	if err := <-done; !errors.Is(err, ErrLength) {
		t.Fatal(err)
	}
	_ = cr.Close()

	// Responder receives a wrong-length msg3 after a valid msg1/msg2 exchange.
	ci2, cr2 := net.Pipe()
	rdone := make(chan error, 1)
	go func() { _, err := Respond(context.Background(), cr2, r, acceptAll, nil); rdone <- err; _ = cr2.Close() }()
	hsI, _ := noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK, Initiator: true,
		Random: rand.Reader, Prologue: []byte(wire.Prologue),
		StaticKeypair: noise.DHKey{Private: i.Scalar(), Public: func() []byte { p := i.Public(); return p[:] }()},
		PeerStatic:    func() []byte { p := r.Public(); return p[:] }()})
	var hs1 wire.HS1
	m1, _, _, _ := hsI.WriteMessage(make([]byte, 2, 2+wire.HS1Len), wire.AppendHS1(nil, &hs1))
	binary.BigEndian.PutUint16(m1, wire.HS1Len)
	_, _ = ci2.Write(m1)
	m2 := make([]byte, wire.HSLenPrefix+wire.HS2Len)
	if _, err := io.ReadFull(ci2, m2); err != nil {
		t.Fatal(err)
	}
	_, _ = ci2.Write([]byte{0x00, 0x70})
	if err := <-rdone; !errors.Is(err, ErrLength) {
		t.Fatal(err)
	}
	_ = ci2.Close()
}

func TestContextCancel(t *testing.T) {
	r := newID(t)
	_, cr := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := Respond(ctx, cr, r, acceptAll, nil); done <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTimeout) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("responder did not unwind on cancel")
	}
}

func TestMsg1Deadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits HandshakeMsg1Wait")
	}
	r := newID(t)
	_, cr := net.Pipe()
	start := time.Now()
	_, err := Respond(context.Background(), cr, r, acceptAll, nil)
	if !errors.Is(err, ErrTimeout) || time.Since(start) > wire.HandshakeMsg1Wait+time.Second {
		t.Fatal(err, time.Since(start))
	}
}

func TestReplayLRU(t *testing.T) {
	l := NewReplayLRU()
	var k [32]byte
	if l.Seen(k) || !l.Seen(k) {
		t.Fatal("basic")
	}
	for i := 1; i <= wire.Msg1ReplayLRU; i++ {
		binary.BigEndian.PutUint32(k[:], uint32(i))
		if l.Seen(k) {
			t.Fatal("fresh key seen")
		}
	}
	// the very first key (all zero) has been evicted
	if l.Seen([32]byte{}) {
		t.Fatal("eviction")
	}
	if len(l.set) != wire.Msg1ReplayLRU {
		t.Fatal(len(l.set))
	}
}

func TestFailureLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	f := NewFailureLimiter(3, time.Minute, 10*time.Minute)
	for i := 0; i < 2; i++ {
		f.Fail("a", now)
	}
	if f.Limited("a", now) || f.Limited("b", now) {
		t.Fatal("limited too early")
	}
	f.Fail("a", now.Add(time.Second))
	if !f.Limited("a", now.Add(time.Second)) || f.Limited("b", now) {
		t.Fatal("not limited")
	}
	if f.Limited("a", now.Add(11*time.Minute)) {
		t.Fatal("penalty did not expire")
	}
	// window slides: two old failures + one new is not three within a minute
	f.Fail("c", now)
	f.Fail("c", now)
	f.Fail("c", now.Add(2*time.Minute))
	if f.Limited("c", now.Add(2*time.Minute)) {
		t.Fatal("window did not slide")
	}
	// bounded keys
	for i := 0; i < maxLimiterKeys+10; i++ {
		f.Fail(string(rune(i))+"x", now.Add(time.Duration(i)*time.Hour))
	}
	if len(f.entries) > maxLimiterKeys {
		t.Fatal(len(f.entries))
	}
}

func BenchmarkHandshake(b *testing.B) {
	i, _ := identity.Generate()
	r, _ := identity.Generate()
	lru := NewReplayLRU()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		ci, cr := net.Pipe()
		done := make(chan struct{})
		go func() {
			res, err := Respond(context.Background(), cr, r, acceptAll, lru)
			if err != nil {
				b.Error(err)
			} else {
				res.Root.Clear()
			}
			close(done)
		}()
		res, err := Initiate(context.Background(), ci, i, r.Public(), nil)
		if err != nil {
			b.Fatal(err)
		}
		res.Root.Clear()
		<-done
		_ = ci.Close()
		_ = cr.Close()
	}
}
