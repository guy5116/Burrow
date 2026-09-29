package core

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

var raceEnabled bool

// rawDial completes an honest handshake to node a with the given token and
// returns the raw peer (no HELLO sent yet).
func rawDial(t *testing.T, a *node, self *identity.Identity, token *[16]byte) (*testpeer.Peer, net.Conn) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(a.port)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	p, err := testpeer.Dial(context.Background(), conn, self, a.id(), token)
	if err != nil {
		t.Fatal(err)
	}
	return p, conn
}

// hello finishes the HELLO exchange; it reports false when the responder
// closed silently instead (rejected initiator).
func hello(p *testpeer.Peer) bool {
	if err := p.Hello(wire.Hello{}); err != nil {
		return false
	}
	in, err := p.Recv(3 * time.Second)
	return err == nil && in.Type == wire.TypeHello
}

// A single-use token is reserved at msg3: a second connection presenting it
// while the first is still in its HELLO exchange is closed silently. A
// handshake that dies before HELLO releases the reservation and burns nothing.
func TestReservedToken(t *testing.T) {
	a := newNode(t, Config{})
	parsed, err := invite.Parse(a.invite(t, false).String)
	if err != nil {
		t.Fatal(err)
	}
	tok := parsed.Token
	first, _ := identity.Generate()
	second, _ := identity.Generate()
	third, _ := identity.Generate()

	p1, c1 := rawDial(t, a, first, &tok) // reserved now; no HELLO yet
	p2, _ := rawDial(t, a, second, &tok)
	if hello(p2) {
		t.Fatal("a reserved single-use token admitted a second initiator")
	}
	if len(a.e.Invites()) != 1 {
		t.Fatal("token consumed before key confirmation")
	}
	// The first dies before HELLO: the reservation is released, nothing is burned.
	_ = c1.Close()
	_ = p1
	deadline := time.Now().Add(15 * time.Second)
	for {
		a.e.mu.Lock()
		n := len(a.e.reserved)
		a.e.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reservation not released after the handshake died")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(a.e.Invites()) != 1 || len(a.e.Contacts()) != 0 {
		t.Fatal("a dead handshake burned the token or created a contact")
	}
	p3, _ := rawDial(t, a, third, &tok)
	if !hello(p3) {
		t.Fatal("token unusable after the reservation was released")
	}
	a.wait(t, "InviteConsumed", isType[InviteConsumed])
	if len(a.e.Invites()) != 0 {
		t.Fatal("single-use token not consumed")
	}
	// Expired token on the responder side: silent close.
	a.e.clockOffset.Store(int64(-2 * time.Hour))
	old, _ := invite.Parse(a.invite(t, false).String)
	a.e.clockOffset.Store(0)
	late, _ := identity.Generate()
	p4, _ := rawDial(t, a, late, &old.Token)
	if hello(p4) {
		t.Fatal("expired token accepted by the responder")
	}
	// Unknown initiator without any token: silent close.
	none, _ := identity.Generate()
	p5, _ := rawDial(t, a, none, nil)
	if hello(p5) {
		t.Fatal("unknown initiator without a token accepted")
	}
}

// A multi-use invite admits at most 64 contacts.
func TestMultiUseInviteCap(t *testing.T) {
	if testing.Short() {
		t.Skip("65 handshakes")
	}
	a := newNode(t, Config{MaxPeers: 128})
	parsed, _ := invite.Parse(a.invite(t, true).String)
	tok := parsed.Token
	for i := 0; i < wire.MaxContactsPerInvite; i++ {
		id, _ := identity.Generate()
		p, conn := rawDial(t, a, id, &tok)
		if !hello(p) {
			t.Fatalf("contact %d rejected", i)
		}
		a.waitN(t, "NewPeerViaInvite", isType[NewPeerViaInvite], i+1)
		_ = conn.Close()
	}
	if len(a.e.Contacts()) != wire.MaxContactsPerInvite {
		t.Fatal(len(a.e.Contacts()))
	}
	extra, _ := identity.Generate()
	p, _ := rawDial(t, a, extra, &tok)
	if hello(p) {
		t.Fatal("65th contact admitted by a multi-use invite")
	}
	if len(a.e.Contacts()) != wire.MaxContactsPerInvite {
		t.Fatal("contact created past the cap")
	}
}

func TestDescribeInviteAndLiveTransport(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	ip, _ := a.e.CreateInvite(InviteOptions{Host: "192.0.2.7", Port: 1, MultiUse: true})
	host, _ := a.e.CreateInvite(InviteOptions{Host: "chat.example", Port: 2})
	onion, _ := a.e.CreateInvite(InviteOptions{Host: strings.Repeat("b", 56) + ".onion", Kind: transport.KindTor, Port: 3})
	for s, want := range map[string]InviteInfo{
		ip.String:    {Host: "192.0.2.7", Port: 1, Kind: transport.KindTCP, MultiUse: true},
		host.String:  {Host: "chat.example", Port: 2, Kind: transport.KindTCP, Hostname: true},
		onion.String: {Host: strings.Repeat("b", 56) + ".onion", Port: 3, Kind: transport.KindTor},
	} {
		got, err := DescribeInvite(s)
		if err != nil || got.Host != want.Host || got.Port != want.Port || got.Kind != want.Kind || got.Hostname != want.Hostname ||
			got.MultiUse != want.MultiUse || got.Peer != a.id() || got.Expiry.IsZero() {
			t.Fatalf("%+v %v", got, err)
		}
	}
	if _, err := DescribeInvite("burrow1:nope"); !errors.Is(err, invite.ErrInvalid) {
		t.Fatal(err)
	}
	connect(t, b, a, a.invite(t, false).String)
	c, _ := b.e.Contact(a.id())
	if !c.Online || c.Transport != transport.KindTCP || b.e.Contacts()[0].Transport != transport.KindTCP {
		t.Fatalf("%+v", c)
	}
	_ = b.e.Disconnect(a.id())
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	if c, _ := b.e.Contact(a.id()); c.Online || c.Transport != "" {
		t.Fatalf("offline contact reports a transport: %+v", c)
	}
}

// Running out of stream ids ends the session with BYE reason 4 and the dialer reconnects.
func TestStreamExhaustionReconnects(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{AutoReconnect: true})
	b.e.rand = func() time.Duration { return 5 * time.Millisecond }
	connect(t, b, a, a.invite(t, false).String)
	b.e.mu.Lock()
	p := b.e.peers[a.id()]
	b.e.mu.Unlock()
	b.e.onStreamsExhausted(p)
	a.wait(t, "peer left", func(ev Event) bool { d, ok := ev.(PeerDisconnected); return ok && d.Reason == "peer left" })
	b.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	if _, err := b.e.SendText(a.id(), "after exhaustion"); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "text", func(ev Event) bool { m, ok := ev.(MessageReceived); return ok && m.Text == "after exhaustion" })
}

func TestResumableListingAndExtensionMismatch(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	if len(b.e.Resumable()) != 0 {
		t.Fatal("phantom partials")
	}
	tr := &transfer{peer: a.id(), size: 10 * wire.ChunkData, format: wire.FormatJPEG}
	tr.id[0] = 9
	if err := b.e.writeMeta(tr, 4); err != nil {
		t.Fatal(err)
	}
	_ = b.e.st.WriteBlob("partials/"+strings.Repeat("ee", 16)+".meta", []byte("junk"))
	got := b.e.Resumable()
	if len(got) != 1 || got[0].Peer != a.id() || got[0].Done != 4*wire.ChunkData || got[0].Size != tr.size || got[0].Format != wire.FormatJPEG {
		t.Fatalf("%+v", got)
	}
	// A PNG renamed to .jpg is refused before anything is offered.
	png, _ := pngFixture(t, 8, 8, false)
	lie := filepath.Join(filepath.Dir(png), "holiday.jpg")
	if err := os.Rename(png, lie); err != nil {
		t.Fatal(err)
	}
	if _, err := a.e.SendImage(context.Background(), b.id(), lie, ""); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "mismatch", func(ev Event) bool {
		f, ok := ev.(TransferFailed)
		return ok && f.Reason == "file extension does not match the image data"
	})
	time.Sleep(30 * time.Millisecond)
	if b.count(isType[ImageOffered]) != 0 {
		t.Fatal("mismatched file was offered")
	}
}

// End-to-end image throughput over loopback TCP including disk writes (§11: ≥ 90 MB/s).
func TestImageThroughput(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("throughput is measured without -short and without the race detector")
	}
	a := imgNode(t, Config{MaxImage: 128 << 20})
	b := imgNode(t, Config{MaxImage: 128 << 20})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 4000, 4000, true) // ≈ 64 MiB of incompressible pixels
	st, _ := os.Stat(path)
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	start := time.Now()
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "TransferDone", isType[TransferDone])
	mbps := float64(st.Size()) / (1 << 20) / time.Since(start).Seconds()
	t.Logf("image transfer: %.0f MiB in %v = %.0f MB/s", float64(st.Size())/(1<<20), time.Since(start).Round(time.Millisecond), mbps)
	if mbps < 90 {
		t.Fatalf("throughput %.0f MB/s is below the 90 MB/s budget", mbps)
	}
}

// The sender's `format` field is untrusted: a peer that announces JPEG but
// sends PNG bytes gets its file saved by what the bytes really are.
func TestReceiverIgnoresDeclaredFormat(t *testing.T) {
	a := imgNode(t, Config{})
	self, _ := identity.Generate()
	parsed, _ := invite.Parse(a.invite(t, false).String)
	p, _ := rawDial(t, a, self, &parsed.Token)
	if err := p.Hello(wire.Hello{Features: wire.FeatureImages, MaxImage: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	if in, err := p.Recv(3 * time.Second); err != nil || in.Type != wire.TypeHello {
		t.Fatal(err)
	}
	path, want := pngFixture(t, 16, 16, false)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := mediaHash(data)
	offer, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: uint64(len(data)), Format: wire.FormatJPEG, Width: 16, Height: 16, Hash: hash})
	if err := p.SendFrame(wire.TypeImgOffer, 2, offer); err != nil {
		t.Fatal(err)
	}
	off := a.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if off.Format != wire.FormatJPEG {
		t.Fatal("the offer should show what the peer claimed")
	}
	if err := a.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	if in, err := p.Recv(3 * time.Second); err != nil || in.Type != wire.TypeImgAccept {
		t.Fatal(err)
	}
	chunk, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: data})
	_ = p.SendFrame(wire.TypeImgChunk, 2, chunk)
	_ = p.SendFrame(wire.TypeImgDone, 2, nil)
	done := a.wait(t, "TransferDone", isType[TransferDone]).(TransferDone)
	if !strings.HasSuffix(done.Path, ".png") {
		t.Fatalf("saved as %s: the declared format was trusted", done.Path)
	}
	got, err := DecodeImage(done.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	samePix(t, want, got)
	// Bytes that are no image at all are refused at save time.
	junk := []byte("this is not an image, whatever the offer says")
	offer2, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: uint64(len(junk)), Format: wire.FormatPNG, Width: 4, Height: 4, Hash: mediaHash(junk)})
	_ = p.SendFrame(wire.TypeImgOffer, 4, offer2)
	off2 := a.wait(t, "second offer", func(ev Event) bool { o, ok := ev.(ImageOffered); return ok && o.ID != off.ID }).(ImageOffered)
	_ = a.e.AcceptImage(off2.ID, "")
	chunk2, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: junk})
	_ = p.SendFrame(wire.TypeImgChunk, 4, chunk2)
	_ = p.SendFrame(wire.TypeImgDone, 4, nil)
	a.wait(t, "cannot save", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "cannot save image" })
}

// The 1,024-contact cap applies to the side that dials as well.
func TestContactCapWhenDialing(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	b.e.mu.Lock()
	for i := 0; i < wire.MaxContacts; i++ {
		var id PeerID
		id[0], id[1], id[2] = 0xEE, byte(i>>8), byte(i)
		b.e.contacts[id] = &Contact{ID: id, Nickname: "filler"}
	}
	b.e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := b.e.Connect(ctx, Target{Invite: a.invite(t, false).String}); !errors.Is(err, ErrContactLimit) {
		t.Fatal(err)
	}
	if len(a.e.Contacts()) != 0 || len(a.e.Invites()) != 1 {
		t.Fatal("a refused dial reached the responder or burned the invite")
	}
	// Invites from a prompt arrive as bytes and are wiped by Connect.
	b.e.mu.Lock()
	b.e.contacts = map[PeerID]*Contact{}
	b.e.mu.Unlock()
	raw := []byte(a.invite(t, false).String)
	if _, err := b.e.Connect(ctx, Target{InviteBytes: raw}); err != nil {
		t.Fatal(err)
	}
	for _, c := range raw {
		if c != 0 {
			t.Fatal("invite bytes not wiped after Connect")
		}
	}
}
