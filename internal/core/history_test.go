package core

import (
	"errors"
	"testing"

	"github.com/guy5116/burrow/internal/identity"
)

func TestHistoryRoundTripAndBurn(t *testing.T) {
	a := newNode(t, Config{History: true})
	b := newNode(t, Config{History: true})
	connect(t, b, a, a.invite(t, false).String)
	other, _ := identity.Generate()
	for i := 0; i < historySegment+5; i++ { // spans two segments
		if _, err := b.e.SendText(a.id(), "m"+itoa(uint16(i))); err != nil {
			t.Fatal(err)
		}
	}
	a.waitN(t, "MessageReceived", isType[MessageReceived], historySegment+5)
	got, err := a.e.History(b.id(), 0)
	if err != nil || len(got) != 100 {
		t.Fatalf("%d %v", len(got), err)
	}
	if got[0].Text != "m"+itoa(uint16(historySegment+5-100)) || got[99].Text != "m"+itoa(uint16(historySegment+4)) || got[0].Mine {
		t.Fatalf("order: %q … %q", got[0].Text, got[99].Text)
	}
	all, _ := a.e.History(b.id(), 10000)
	if len(all) != historySegment+5 {
		t.Fatal(len(all))
	}
	mine, _ := b.e.History(a.id(), 3)
	if len(mine) != 3 || !mine[0].Mine || mine[2].Text != "m"+itoa(uint16(historySegment+4)) || mine[0].At.IsZero() {
		t.Fatalf("%+v", mine)
	}
	if h, _ := a.e.History(other.Public(), 5); len(h) != 0 {
		t.Fatal("history for a stranger")
	}
	// Segment files are opaque and encrypted; nothing names the peer.
	blobs, _ := a.e.st.ListBlobs("history")
	if len(blobs) != 3 {
		t.Fatal(blobs)
	}
	// Restart the engine over the same store: history persists.
	e2, err := New(Config{History: true}, a.e.st, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := e2.History(b.id(), 2); len(h) != 2 {
		t.Fatal("history lost after restart")
	}
	e2.Close()
	n, err := a.e.Burn()
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	if h, _ := a.e.History(b.id(), 5); len(h) != 0 {
		t.Fatal("history survived burn")
	}
	// Off by default.
	c := newNode(t, Config{})
	if _, err := c.e.History(a.id(), 1); !errors.Is(err, ErrHistoryOff) {
		t.Fatal(err)
	}
}

func TestMDNSMatch(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	_ = b.e.Disconnect(a.id())
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	var nonce [16]byte
	nonce[0] = 5
	tag := identity.MDNSTag(nonce, a.id())
	id, ok := b.e.matchAnnouncement(nonce, tag)
	if !ok || id != a.id() {
		t.Fatal("contact not recognized")
	}
	if _, ok := b.e.matchAnnouncement(nonce, tag); ok {
		t.Fatal("same nonce dialed twice")
	}
	var other [16]byte
	other[1] = 1
	if _, ok := b.e.matchAnnouncement(other, tag); ok {
		t.Fatal("tag for a different nonce matched")
	}
	stranger, _ := identity.Generate()
	if _, ok := b.e.matchAnnouncement(other, identity.MDNSTag(other, stranger.Public())); ok {
		t.Fatal("stranger matched")
	}
}
