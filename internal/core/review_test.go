package core

// Regression tests for defects found in review. Each one failed before its fix.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/wire"
)

// stranger connects a scriptable peer with a fresh identity through a new invite.
func stranger(t *testing.T, a *node) (*testpeer.Peer, *identity.Identity) {
	t.Helper()
	self, _ := identity.Generate()
	parsed, _ := invite.Parse(a.invite(t, false).String)
	p, _ := rawDial(t, a, self, &parsed.Token)
	if !hello(p) {
		t.Fatal("not admitted")
	}
	return p, self
}

func offersOf(n *node) (out []ImageOffered) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, ev := range n.events {
		if o, ok := ev.(ImageOffered); ok {
			out = append(out, o)
		}
	}
	return out
}

// A message sent again after a reconnect is shown once: the set of ids seen
// belongs to the contact, not to the session.
func TestDedupSurvivesReconnect(t *testing.T) {
	a := newNode(t, Config{})
	p, self := stranger(t, a)
	a.wait(t, "PeerConnected", isType[PeerConnected])
	text, _ := wire.AppendText(nil, wire.Text{MsgID: 42, Text: []byte("once")})
	send := func(p *testpeer.Peer) {
		t.Helper()
		if err := p.SendFrame(wire.TypeText, wire.StreamChat, text); err != nil {
			t.Fatal(err)
		}
		recvType(t, p, wire.TypeAck) // acknowledged every time
	}
	send(p)
	_ = p.Conn.Close()
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	p2, _ := rawDial(t, a, self, nil)
	if !hello(p2) {
		t.Fatal("known contact rejected")
	}
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	send(p2)
	if n := a.count(isType[MessageReceived]); n != 1 {
		t.Fatalf("shown %d times", n)
	}
}

// BYE "replaced" on the only session we have is a disconnect like any other.
func TestByeReplacedWithoutAnotherSession(t *testing.T) {
	a := newNode(t, Config{})
	p, _ := stranger(t, a)
	a.wait(t, "PeerConnected", isType[PeerConnected])
	if err := p.SendFrame(wire.TypeBye, wire.StreamControl, []byte{wire.ByeReplaced}); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	if a.peerCount() != 0 || a.e.Contacts()[0].Online {
		t.Fatal("still shown as connected")
	}
}

// A peer that dials again right after losing its connection replaces its own
// old session; the rule for simultaneous dials is not for this.
func TestRedialInsideGlareWindow(t *testing.T) {
	a := newNode(t, Config{})
	_, self := stranger(t, a)
	a.wait(t, "PeerConnected", isType[PeerConnected])
	p2, _ := rawDial(t, a, self, nil)
	if !hello(p2) {
		t.Fatal("no hello on the second session")
	}
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	if a.peerCount() != 1 || a.peerOf(self.Public()).s.Initiator() {
		t.Fatal("the new session did not take the place of the old one")
	}
	if _, err := a.e.SendText(self.Public(), "to the new session"); err != nil {
		t.Fatal(err)
	}
	recvType(t, p2, wire.TypeText)
}

// The same image offered twice and accepted twice arrives twice: the second
// download must not take over the partial the first is still writing.
func TestSameImageAcceptedTwice(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{ChunkDelay: 5 * time.Millisecond})
	connect(t, b, a, a.invite(t, false).String)
	path, want := pngFixture(t, 600, 600, true)
	for n := 0; n < 2; n++ {
		if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
			t.Fatal(err)
		}
	}
	b.waitN(t, "offers", isType[ImageOffered], 2)
	for _, o := range offersOf(b) {
		if err := b.e.AcceptImage(o.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	b.waitN(t, "TransferDone", isType[TransferDone], 2)
	if b.count(isType[TransferFailed])+b.count(isType[TransferResumed]) != 0 {
		t.Fatalf("events: %s", b.dump())
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ev := range b.events {
		if d, ok := ev.(TransferDone); ok {
			got, err := DecodeImage(context.Background(), d.Path, 0)
			if err != nil {
				t.Fatal(err)
			}
			samePix(t, want, got)
		}
	}
}

// With two downloads running, a third accept is refused and the offer stays,
// so it can be accepted once there is room.
func TestThirdAcceptWaits(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{ChunkDelay: 10 * time.Millisecond})
	connect(t, b, a, a.invite(t, false).String)
	for n := 0; n < 3; n++ {
		path, _ := pngFixture(t, 500+n, 500, true)
		if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
			t.Fatal(err)
		}
	}
	b.waitN(t, "offers", isType[ImageOffered], 3)
	offers := offersOf(b)
	for _, o := range offers[:2] {
		if err := b.e.AcceptImage(o.ID, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.e.AcceptImage(offers[2].ID, ""); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*.part")); len(parts) != 2 {
		t.Fatalf("%d partials for two downloads", len(parts))
	}
	b.waitN(t, "TransferDone", isType[TransferDone], 1)
	if err := b.e.AcceptImage(offers[2].ID, ""); err != nil {
		t.Fatal(err)
	}
	b.waitN(t, "TransferDone", isType[TransferDone], 3)
	if b.count(isType[TransferFailed]) != 0 {
		t.Fatal(b.dump())
	}
}

// Cancelling a send while the file is still being hashed takes no stream id:
// after more such cancels than there are offer slots, sending still works.
func TestCancelWhileHashing(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a large file")
	}
	a := imgNode(t, Config{MaxFile: 1 << 30})
	b := imgNode(t, Config{MaxFile: 1 << 30})
	connect(t, b, a, a.invite(t, false).String)
	big := filepath.Join(t.TempDir(), "big.dat")
	if err := os.WriteFile(big, make([]byte, 200<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < wire.MaxPendingOffers+1; n++ {
		id, err := a.e.SendFile(context.Background(), b.id(), big, "")
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := a.e.CancelTransfer(id); err != nil {
			t.Fatal(err)
		}
		a.waitN(t, "cancelled", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.ID == id }, 1)
		if took := time.Since(start); took > time.Second {
			t.Fatalf("cancel took %v: the hashing was not interrupted", took)
		}
	}
	small, _ := fileFixture(t, "small.zip", 100)
	if _, err := a.e.SendFile(context.Background(), b.id(), small, ""); err != nil {
		t.Fatal(err)
	}
	if o := b.wait(t, "offer", isType[ImageOffered]).(ImageOffered); o.Size != 100 || b.count(isType[ImageOffered]) != 1 {
		t.Fatalf("%+v", o)
	}
}

// Handshakes that run side by side cannot take a multi-use invite past its cap.
func TestMultiUseCapWithConcurrentHandshakes(t *testing.T) {
	a := newNode(t, Config{})
	parsed, _ := invite.Parse(a.invite(t, true).String)
	tok := parsed.Token
	a.e.do(func() { a.e.invites[tok].Uses = wire.MaxContactsPerInvite - 1 })
	x, _ := identity.Generate()
	y, _ := identity.Generate()
	p1, _ := rawDial(t, a, x, &tok)
	p2, _ := rawDial(t, a, y, &tok)
	time.Sleep(300 * time.Millisecond) // both have passed msg3 before either says HELLO
	hello(p1)
	hello(p2)
	a.wait(t, "NewPeerViaInvite", isType[NewPeerViaInvite])
	time.Sleep(200 * time.Millisecond)
	uses := 0
	a.e.do(func() { uses = a.e.invites[tok].Uses })
	if uses != wire.MaxContactsPerInvite || len(a.e.Contacts()) != 1 {
		t.Fatalf("used %d times, %d contacts", uses, len(a.e.Contacts()))
	}
}

// A contact that presents someone else's reserved token does not release it.
func TestKnownContactCannotReleaseReservation(t *testing.T) {
	a := newNode(t, Config{})
	pk, known := stranger(t, a)
	a.wait(t, "PeerConnected", isType[PeerConnected])
	_ = pk.Conn.Close()
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])

	parsed, _ := invite.Parse(a.invite(t, false).String)
	tok := parsed.Token
	x, _ := identity.Generate()
	_, _ = rawDial(t, a, x, &tok) // x holds the reservation; its HELLO is outstanding
	reservedFor := func() (p PeerID, ok bool) {
		a.e.do(func() { p, ok = a.e.reserved[tok] })
		return p, ok
	}
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if p, ok := reservedFor(); ok && p == x.Public() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not reserved")
		}
	}
	pk2, _ := rawDial(t, a, known, &tok)
	if !hello(pk2) {
		t.Fatal("known contact rejected")
	}
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	if p, ok := reservedFor(); !ok || p != x.Public() {
		t.Fatal("the reservation was released by a contact that does not hold it")
	}
	y, _ := identity.Generate()
	if py, _ := rawDial(t, a, y, &tok); hello(py) {
		t.Fatal("a second stranger was let in on a reserved token")
	}
}

// Removing a contact removes what was queued for it, connected or not.
func TestRemoveContactDropsQueue(t *testing.T) {
	a := newNode(t, Config{})
	_, self := stranger(t, a)
	a.wait(t, "PeerConnected", isType[PeerConnected])
	if _, err := a.e.SendText(self.Public(), "for the removed contact"); err != nil {
		t.Fatal(err)
	}
	if err := a.e.RemoveContact(self.Public()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for a.peerCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("session still open")
		}
		time.Sleep(10 * time.Millisecond)
	}
	queued, remembered := 0, false
	a.e.do(func() {
		queued = len(a.e.orphanQueues[self.Public()])
		_, remembered = a.e.dedup[self.Public()]
	})
	if queued != 0 || remembered || a.count(isType[PeerDisconnected]) != 0 {
		t.Fatalf("queued=%d remembered=%v events=%s", queued, remembered, a.dump())
	}
}

// A receiver that cancels while its disk is behind keeps reading the session.
func TestSessionAliveAfterCancelledDownload(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{ChunkDelay: 30 * time.Millisecond})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 1200, 1200, true)
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "progress", isType[TransferProgress])
	time.Sleep(50 * time.Millisecond) // the chunk queue is full by now
	if err := b.e.CancelTransfer(off.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.e.SendText(b.id(), "are you there"); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "text", isType[MessageReceived])
	deadline := time.Now().Add(3 * time.Second)
	for {
		parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*"))
		metas, _ := b.e.st.ListBlobs(partialsDir)
		if len(parts)+len(metas) == 0 {
			return // a cancelled download leaves nothing behind
		}
		if time.Now().After(deadline) {
			t.Fatal(parts, metas)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
