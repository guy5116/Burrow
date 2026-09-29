package core

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/tcp"
	"github.com/guy5116/burrow/internal/wire"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var fast = store.Options{Argon2: store.Argon2{Time: 1, MemKiB: store.MinMemKiB, Threads: 1}}

// node is one engine under test with an event recorder.
type node struct {
	e      *Engine
	st     *store.Store
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	events []Event
	cond   *sync.Cond
	port   uint16
}

func newNode(t *testing.T, cfg Config) *node {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Init(dir, []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddr = "127.0.0.1:0"
	e, err := New(cfg, st, []transport.Transport{tcp.New(cfg.ListenAddr)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{e: e, st: st, cancel: cancel, done: make(chan struct{})}
	n.cond = sync.NewCond(&n.mu)
	go func() { _ = e.Start(ctx); close(n.done) }()
	go func() {
		for {
			select {
			case ev := <-e.Events():
				n.mu.Lock()
				n.events = append(n.events, ev)
				n.cond.Broadcast()
				n.mu.Unlock()
			case <-e.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-n.done
		st.Close()
	})
	deadline := time.Now().Add(3 * time.Second)
	for len(e.ListenAddrs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("engine did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	n.port = uint16(e.ListenAddrs()[0].(*net.TCPAddr).Port)
	return n
}

// wait blocks until an event satisfying pred has been recorded (or fails after timeout).
func (n *node) wait(t *testing.T, what string, pred func(Event) bool) Event {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	n.mu.Lock()
	defer n.mu.Unlock()
	i := 0
	for {
		for ; i < len(n.events); i++ {
			if pred(n.events[i]) {
				return n.events[i]
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s; events: %s", what, n.dump())
		}
		timer := time.AfterFunc(100*time.Millisecond, n.cond.Broadcast)
		n.cond.Wait()
		timer.Stop()
	}
}

func (n *node) dump() string {
	var sb strings.Builder
	for _, ev := range n.events {
		sb.WriteString(reflect.TypeOf(ev).Name())
		if f, ok := ev.(TransferFailed); ok {
			sb.WriteString("(" + f.Reason + ")")
		}
		sb.WriteByte(' ')
	}
	return sb.String()
}

func (n *node) count(pred func(Event) bool) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.countLocked(pred)
}

func (n *node) countLocked(pred func(Event) bool) int {
	c := 0
	for _, ev := range n.events {
		if pred(ev) {
			c++
		}
	}
	return c
}

// waitN blocks until at least want events satisfy pred.
func (n *node) waitN(t *testing.T, what string, pred func(Event) bool, want int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	n.mu.Lock()
	defer n.mu.Unlock()
	for n.countLocked(pred) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %d× %s; events: %s", want, what, n.dump())
		}
		timer := time.AfterFunc(100*time.Millisecond, n.cond.Broadcast)
		n.cond.Wait()
		timer.Stop()
	}
}

func isType[T Event](ev Event) bool { _, ok := ev.(T); return ok }

func (n *node) id() PeerID { return n.e.Identity().ID }

func (n *node) invite(t *testing.T, multi bool) Invite {
	t.Helper()
	inv, err := n.e.CreateInvite(InviteOptions{Host: "127.0.0.1", Port: n.port, MultiUse: multi})
	if err != nil {
		t.Fatal(err)
	}
	return inv
}

func connect(t *testing.T, from, to *node, inv string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := from.e.Connect(ctx, Target{Invite: inv})
	if err != nil {
		t.Fatal(err)
	}
	if id != to.id() {
		t.Fatal("peer id")
	}
	from.wait(t, "PeerConnected", func(ev Event) bool { pc, ok := ev.(PeerConnected); return ok && pc.Peer == to.id() })
	to.wait(t, "PeerConnected", func(ev Event) bool { pc, ok := ev.(PeerConnected); return ok && pc.Peer == from.id() })
}

func TestInviteFlowTextAckTyping(t *testing.T) {
	a := newNode(t, Config{DisplayName: "Alice", Typing: true})
	b := newNode(t, Config{DisplayName: "Bob", Typing: true})
	inv := a.invite(t, false)
	if inv.String == "" || !strings.HasPrefix(inv.String, "burrow1:") {
		t.Fatal("invite string")
	}
	connect(t, b, a, inv.String)
	// Both sides created contacts with sanitized HELLO names; A consumed the token.
	a.wait(t, "NewPeerViaInvite", isType[NewPeerViaInvite])
	a.wait(t, "InviteConsumed", func(ev Event) bool { ic, ok := ev.(InviteConsumed); return ok && ic.ID == inv.ID })
	b.wait(t, "NewPeerViaInvite", isType[NewPeerViaInvite])
	ca, _ := a.e.Contact(b.id())
	cb, _ := b.e.Contact(a.id())
	if ca.Nickname != "Bob" || cb.Nickname != "Alice" || ca.Verified || !ca.Online || len(cb.Addrs) != 1 {
		t.Fatalf("contacts %+v %+v", ca, cb)
	}
	if len(a.e.Invites()) != 0 {
		t.Fatal("single-use invite not consumed")
	}
	// Text with status transitions and ACK.
	mid, err := b.e.SendText(a.id(), "hello  \u202E world")
	if err != nil {
		t.Fatal(err)
	}
	got := a.wait(t, "MessageReceived", isType[MessageReceived]).(MessageReceived)
	if got.Peer != b.id() || got.ID != mid || got.Text != "hello  � world" || got.SentAt.IsZero() {
		t.Fatalf("%+v", got)
	}
	for _, st := range []Status{StatusPending, StatusSent, StatusDelivered} {
		b.wait(t, st.String(), func(ev Event) bool { ms, ok := ev.(MessageStatus); return ok && ms.ID == mid && ms.Status == st })
	}
	if q := b.e.Queue(a.id()); len(q) != 0 {
		t.Fatal("delivered message still queued")
	}
	// Reverse direction.
	if _, err := a.e.SendText(b.id(), "yo"); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "MessageReceived", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.Text == "yo" })
	// Typing both enabled.
	b.e.SetTyping(a.id(), true)
	a.wait(t, "Typing", func(ev Event) bool { ty, ok := ev.(Typing); return ok && ty.Typing })
	// Safety numbers agree.
	sa, _ := a.e.SafetyNumber(b.id())
	sb, _ := b.e.SafetyNumber(a.id())
	if sa != sb {
		t.Fatal("safety numbers differ")
	}
	if err := b.e.VerifyContact(a.id()); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "PeerVerified", isType[PeerVerified])
	if err := b.e.RenameContact(a.id(), " Al\u200Dice \n"); err != nil {
		t.Fatal(err)
	}
	if c, _ := b.e.Contact(a.id()); c.Nickname != "Alice" || !c.Verified {
		t.Fatalf("%+v", c)
	}
	// Persisted across engine restart.
	b.cancel()
	<-b.done
	st2, err := store.Open(t.TempDir(), nil)
	if err == nil {
		st2.Close()
		t.Fatal("unexpected store")
	}
	e2, err := New(Config{}, b.st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := e2.Contact(a.id()); err != nil || c.Nickname != "Alice" || !c.Verified {
		t.Fatalf("%+v %v", c, err)
	}
	e2.Close()
	// A saw B leave with BYE.
	a.wait(t, "PeerDisconnected", func(ev Event) bool { pd, ok := ev.(PeerDisconnected); return ok && pd.Reason == "peer left" })
}

func TestInviteRules(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	c := newNode(t, Config{})
	// Single-use: second stranger is rejected silently (HandshakeFailed at hello on the initiator).
	inv := a.invite(t, false)
	connect(t, b, a, inv.String)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.e.Connect(ctx, Target{Invite: inv.String}); err == nil {
		t.Fatal("consumed invite accepted")
	}
	c.wait(t, "HandshakeFailed", func(ev Event) bool { hf, ok := ev.(HandshakeFailed); return ok && hf.Stage == "hello" })
	// A stranger with no token: silent.
	if _, err := c.e.Connect(ctx, Target{Contact: ptr(a.id())}); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	// Known contact reconnects without a token, even after the invite is gone.
	if err := b.e.Disconnect(a.id()); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err != nil {
		t.Fatal(err)
	}
	// Multi-use invite admits several peers and stays valid.
	multi := a.invite(t, true)
	connect(t, c, a, multi.String)
	d := newNode(t, Config{})
	connect(t, d, a, multi.String)
	if len(a.e.Invites()) != 1 {
		t.Fatal("multi-use invite consumed")
	}
	if a.count(func(ev Event) bool { ic, ok := ev.(InviteConsumed); return ok && ic.ID == multi.ID }) != 2 {
		t.Fatal("InviteConsumed per use")
	}
	if err := a.e.RevokeInvite(multi.ID); err != nil || len(a.e.Invites()) != 0 {
		t.Fatal(err)
	}
	// Malformed and expired invites never touch the network.
	if _, err := c.e.Connect(ctx, Target{Invite: "burrow1:nope"}); !errors.Is(err, invite.ErrInvalid) {
		t.Fatal(err)
	}
	a.e.clockOffset.Store(int64(-2 * time.Hour))
	expired := a.invite(t, false)
	a.e.clockOffset.Store(0)
	if _, err := c.e.Connect(ctx, Target{Invite: expired.String}); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatal(err)
	}
	// Wrong responder key: silent close, HandshakeFailed at msg2.
	other, _ := identity.Generate()
	bad := invite.Invite{Kind: invite.KindTCP, Addr: "127.0.0.1", Port: a.port, PubKey: other.Public(), Expiry: time.Now().Add(time.Hour)}
	badStr, _ := bad.Encode()
	e := newNode(t, Config{})
	if _, err := e.e.Connect(ctx, Target{Invite: badStr}); err == nil {
		t.Fatal("wrong key accepted")
	}
	e.wait(t, "HandshakeFailed msg2", func(ev Event) bool { hf, ok := ev.(HandshakeFailed); return ok && hf.Stage == "msg2" })
}

func ptr(id PeerID) *PeerID { return &id }

func TestBlockAndRemove(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	if err := a.e.BlockContact(b.id(), true); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err == nil {
		t.Fatal("blocked peer connected")
	}
	if _, err := a.e.Connect(ctx, Target{Contact: ptr(b.id())}); !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
	if _, err := a.e.SendText(b.id(), "x"); !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
	if err := a.e.BlockContact(b.id(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err != nil {
		t.Fatal(err)
	}
	if err := a.e.RemoveContact(b.id()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.e.Contact(b.id()); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	b.wait(t, "PeerDisconnected #2", func(ev Event) bool { return isType[PeerDisconnected](ev) }) // at least one
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err == nil {
		t.Fatal("removed contact accepted without invite")
	}
}

// Queue while offline, then resend in order with the original ids after the next HELLO;
// the receiver dedups and re-ACKs.
func TestQueueOfflineAndDedup(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	if err := b.e.Disconnect(a.id()); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	var ids []MsgID
	for i := 0; i < 3; i++ {
		id, err := b.e.SendText(a.id(), "queued "+string(rune('0'+i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if q := b.e.Queue(a.id()); len(q) != 3 || q[0].Status != StatusPending {
		t.Fatalf("%+v", q)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		b.wait(t, "delivered", func(ev Event) bool {
			ms, ok := ev.(MessageStatus)
			return ok && ms.ID == id && ms.Status == StatusDelivered
		})
		a.wait(t, "received", func(ev Event) bool {
			m, ok := ev.(MessageReceived)
			return ok && m.ID == id && m.Text == "queued "+string(rune('0'+i))
		})
	}
	// Order preserved on the receiver.
	a.mu.Lock()
	var seen []MsgID
	for _, ev := range a.events {
		if m, ok := ev.(MessageReceived); ok {
			seen = append(seen, m.ID)
		}
	}
	a.mu.Unlock()
	if !reflect.DeepEqual(seen, ids) {
		t.Fatalf("order %v want %v", seen, ids)
	}
	if len(b.e.Queue(a.id())) != 0 {
		t.Fatal("queue not drained")
	}
	if _, err := b.e.SendText(a.id(), ""); err == nil {
		t.Fatal("empty message accepted")
	}
}

// A raw peer that never ACKs: sent messages revert to pending on disconnect
// and are resent with the same ids on the next session; duplicates are ACKed
// again but delivered once.
func TestSentRevertsToPendingAndResend(t *testing.T) {
	a := newNode(t, Config{})
	self, _ := identity.Generate()
	inv := a.invite(t, false)
	parsed, _ := invite.Parse(inv.String)
	dial := func() *testpeer.Peer {
		conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(a.port)))
		if err != nil {
			t.Fatal(err)
		}
		tok := parsed.Token
		var tp *[16]byte
		if len(a.e.Invites()) > 0 {
			tp = &tok
		}
		p, err := testpeer.Dial(context.Background(), conn, self, a.id(), tp)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Hello(wire.Hello{}); err != nil {
			t.Fatal(err)
		}
		if in, err := p.Recv(3 * time.Second); err != nil || in.Type != wire.TypeHello {
			t.Fatal(in, err)
		}
		return p
	}
	p := dial()
	a.wait(t, "PeerConnected", isType[PeerConnected])
	id, err := a.e.SendText(self.Public(), "unacked")
	if err != nil {
		t.Fatal(err)
	}
	in, err := p.Recv(3 * time.Second)
	if err != nil || in.Type != wire.TypeText {
		t.Fatal(in, err)
	}
	a.wait(t, "sent", func(ev Event) bool { ms, ok := ev.(MessageStatus); return ok && ms.ID == id && ms.Status == StatusSent })
	_ = p.Conn.Close()
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	if q := a.e.Queue(self.Public()); len(q) != 1 || q[0].Status != StatusPending {
		t.Fatalf("%+v", q)
	}
	a.wait(t, "reverted", func(ev Event) bool {
		ms, ok := ev.(MessageStatus)
		return ok && ms.ID == id && ms.Status == StatusPending
	})
	// Reconnect: resent with the same id.
	p = dial()
	in, err = p.Recv(3 * time.Second)
	if err != nil || in.Type != wire.TypeText {
		t.Fatal(in, err)
	}
	tx, _ := wire.DecodeText(in.Payload)
	if tx.MsgID != uint64(id) || string(tx.Text) != "unacked" {
		t.Fatalf("%+v", tx)
	}
	if err := p.SendFrame(wire.TypeAck, 1, wire.AppendAck(nil, wire.Ack{MsgID: tx.MsgID, Status: 1})); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "delivered", func(ev Event) bool {
		ms, ok := ev.(MessageStatus)
		return ok && ms.ID == id && ms.Status == StatusDelivered
	})
	// Duplicate TEXT from the peer: one MessageReceived, two ACKs. Unknown ACK ignored.
	txt, _ := wire.AppendText(nil, wire.Text{MsgID: 42, SentAt: 0, Text: []byte("dup")})
	for i := 0; i < 2; i++ {
		if err := p.SendFrame(wire.TypeText, 1, txt); err != nil {
			t.Fatal(err)
		}
		in, err := p.Recv(3 * time.Second)
		if err != nil || in.Type != wire.TypeAck {
			t.Fatal(in, err)
		}
	}
	if err := p.SendFrame(wire.TypeAck, 1, wire.AppendAck(nil, wire.Ack{MsgID: 999, Status: 1})); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "dup received", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.ID == 42 && m.SentAt.IsZero() })
	time.Sleep(50 * time.Millisecond)
	if a.count(func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.ID == 42 }) != 1 {
		t.Fatal("duplicate delivered twice")
	}
	_ = p.Conn.Close()
}

func itoa(n uint16) string { return strings.TrimSpace(strings.Repeat(" ", 0) + string(fmtInt(int(n)))) }

func fmtInt(n int) []byte {
	if n == 0 {
		return []byte("0")
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return b
}

func TestSessionReplacement(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	// Pretend the first session is old on both sides so this is a replacement, not glare.
	for _, n := range []*node{a, b} {
		n.e.clockOffset.Store(int64(10 * time.Second))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.e.Connect(ctx, Target{Contact: ptr(a.id())}); err != nil {
		t.Fatal(err)
	}
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	time.Sleep(100 * time.Millisecond)
	if a.count(isType[PeerDisconnected]) != 0 || b.count(isType[PeerDisconnected]) != 0 {
		t.Fatalf("replacement emitted PeerDisconnected: a=%s b=%s", a.dump(), b.dump())
	}
	na := a.peerCount()
	nb := b.peerCount()
	if na != 1 || nb != 1 {
		t.Fatal(na, nb)
	}
	if _, err := b.e.SendText(a.id(), "after replace"); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "text", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.Text == "after replace" })
}

func TestSimultaneousDialGlare(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	_ = b.e.Disconnect(a.id())
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	// Give A B's address, then dial both ways at once.
	a.e.do(func() {
		a.e.rememberAddrL(b.id(), transport.Address{Kind: transport.KindTCP, Host: "127.0.0.1", Port: b.port})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, pair := range [][2]*node{{a, b}, {b, a}} {
		wg.Add(1)
		go func(from, to *node) {
			defer wg.Done()
			_, _ = from.e.Connect(ctx, Target{Contact: ptr(to.id())})
		}(pair[0], pair[1])
	}
	wg.Wait()
	// Settle: exactly one live session on each side, and it works.
	deadline := time.Now().Add(5 * time.Second)
	for {
		na := a.peerCount()
		nb := b.peerCount()
		if na == 1 && nb == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not settle: %d %d", na, nb)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := a.e.SendText(b.id(), "glare ok"); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "text", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.Text == "glare ok" })
	// Which session survived depends on timing: the two dials may not have
	// overlapped, or the sides may have healed by reconnecting (§12). The rule
	// itself is checked in TestGlareRule.
}

func TestGlareRule(t *testing.T) {
	small, big := PeerID{1}, PeerID{2}
	for _, c := range []struct {
		name               string
		self, peer         PeerID
		newInit, oldInit   bool
		age                time.Duration
		newSessionGivesWay bool
	}{
		{"we dialed second and have the smaller key", small, big, true, false, time.Second, false},
		{"we dialed second and have the larger key", big, small, true, false, time.Second, true},
		{"they dialed second and have the smaller key", big, small, false, true, time.Second, false},
		{"they dialed second and have the larger key", small, big, false, true, time.Second, true},
		{"outside the window the newer session wins", big, small, true, false, wire.GlareWindow, false},
		{"a redial by us", big, small, true, true, time.Second, false},
		{"a redial by them", small, big, false, false, time.Second, false},
	} {
		if got := losesGlare(c.self, c.peer, c.newInit, c.oldInit, c.age); got != c.newSessionGivesWay {
			t.Errorf("%s: %v", c.name, got)
		}
		// The other side looks at the same two sessions and must agree.
		if got := losesGlare(c.peer, c.self, !c.newInit, !c.oldInit, c.age); got != c.newSessionGivesWay {
			t.Errorf("%s: the two sides disagree", c.name)
		}
	}
}

func TestNameCollision(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{DisplayName: "Sam"})
	c := newNode(t, Config{DisplayName: "ＳＡＭ"}) // normalizes to "sam"
	connect(t, b, a, a.invite(t, false).String)
	connect(t, c, a, a.invite(t, false).String)
	ev := a.wait(t, "NameCollision", isType[NameCollision]).(NameCollision)
	if ev.New != c.id() || ev.Existing != b.id() {
		t.Fatalf("%+v", ev)
	}
	cc, _ := a.e.Contact(c.id())
	if cc.Nickname != "ＳＡＭ ("+c.id().Short()+")" {
		t.Fatalf("%q", cc.Nickname)
	}
}

func TestAutoReconnect(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{AutoReconnect: true})
	b.e.rand = func() time.Duration { return 10 * time.Millisecond } // small jitter
	connect(t, b, a, a.invite(t, false).String)
	// A drops the connection without BYE (simulate a crash by closing the raw session).
	pa := a.peerOf(b.id())
	pa.s.Close(wire.ByeLocalError) // BYE reason 2: peer did not cause it → B still reconnects
	b.wait(t, "Reconnecting", isType[Reconnecting])
	b.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	if _, err := b.e.SendText(a.id(), "back"); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "text", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.Text == "back" })
}

func TestFailureRateLimit(t *testing.T) {
	a := newNode(t, Config{})
	for i := 0; i < wire.TCPFailuresPerMinute; i++ {
		conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(a.port)))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte{0, 1}) // wrong msg1 length → failure
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Read(make([]byte, 1))
		_ = conn.Close()
	}
	// Now even an honest peer is accept-and-closed.
	b := newNode(t, Config{})
	inv := a.invite(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.e.Connect(ctx, Target{Invite: inv.String}); err == nil {
		t.Fatal("rate-limited source accepted")
	}
}

func TestGracefulShutdownSendsBye(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	b.cancel()
	<-b.done
	pd := a.wait(t, "PeerDisconnected", isType[PeerDisconnected]).(PeerDisconnected)
	if pd.Reason != "peer left" {
		t.Fatalf("reason %q", pd.Reason)
	}
	if _, err := b.e.Connect(context.Background(), Target{Contact: ptr(a.id())}); !errors.Is(err, ErrNotRunning) {
		t.Fatal("connect after shutdown", err)
	}
}
