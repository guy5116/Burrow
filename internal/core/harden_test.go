package core

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/handshake"
	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/media"
	"github.com/guy5116/burrow/internal/session"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

func TestSmallMappings(t *testing.T) {
	for err, want := range map[error]string{
		media.ErrUnsupported: "unsupported image format", media.ErrTooLarge: "too large: over your size limit or the one your contact allows", media.ErrEmpty: "file is empty",
		media.ErrAnimated: "animated WebP is not supported", media.ErrCorrupt: "image file is malformed",
		errAnimatedUnsupported: errAnimatedUnsupported.Error(), os.ErrNotExist: "file not found",
		errors.New("x"): "could not read image",
	} {
		if got := redactMediaErr(err); got != want {
			t.Errorf("redactMediaErr(%v)=%q", err, got)
		}
	}
	for err, want := range map[error]bool{nil: false, session.ErrClosed: false, session.ErrProtocol: false,
		&session.ByeError{Reason: wire.ByeUserQuit}: false, &session.ByeError{Reason: wire.ByeShutdown}: false,
		&session.ByeError{Reason: wire.ByeReplaced}: true, &session.ByeError{Reason: wire.ByeLocalError}: true,
		&session.ByeError{Reason: wire.ByeResourceLimit}: true, errors.New("reset by peer"): true} {
		if shouldReconnect(err) != want {
			t.Errorf("shouldReconnect(%v)", err)
		}
	}
	for err, want := range map[error]string{nil: "closed", session.ErrClosed: "closed", session.ErrProtocol: "protocol violation",
		&session.ByeError{}: "peer left", errors.New("eof"): "connection lost"} {
		if disconnectReason(err) != want {
			t.Errorf("disconnectReason(%v)", err)
		}
	}
	if kindOf(2) != transport.KindTor || kindOf(1) != transport.KindTCP {
		t.Fatal("kindOf")
	}
	if reasonOf(session.ErrProtocol) != "protocol violation" || !strings.Contains(reasonOf(errors.New("x")), "fresh invite") {
		t.Fatal("reasonOf")
	}
	if stageOf(&handshake.Error{Stage: "msg3", Err: handshake.ErrLength}) != "msg3" || stageOf(errors.New("x")) != "unknown" {
		t.Fatal("stageOf")
	}
	for r, want := range map[uint8]string{0: "declined", 1: "too large", 2: "unsupported", 3: "busy"} {
		if rejectReason(r) != want {
			t.Errorf("rejectReason(%d)", r)
		}
	}
	if resultReason(1) != "a hash mismatch" || resultReason(2) != "an abort" {
		t.Fatal("resultReason")
	}
	for s, want := range map[Status]string{StatusPending: "pending", StatusSent: "sent", StatusDelivered: "delivered", StatusFailed: "failed"} {
		if s.String() != want {
			t.Errorf("Status(%d)", s)
		}
	}
	c := Config{}
	if c.listenPort() != wire.DefaultListenPort || c.maxPeers() != wire.MaxPeers || c.maxImage() != wire.DefaultMaxImage {
		t.Fatal("defaults")
	}
	c = Config{ListenPort: 9, MaxPeers: 3, MaxImage: 7}
	if c.listenPort() != 9 || c.maxPeers() != 3 || c.maxImage() != 7 {
		t.Fatal("overrides")
	}
}

func TestAPIErrorsForUnknownContacts(t *testing.T) {
	a := newNode(t, Config{})
	stranger, _ := identity.Generate()
	id := stranger.Public()
	for name, err := range map[string]error{
		"rename": a.e.RenameContact(id, "x"), "verify": a.e.VerifyContact(id), "block": a.e.BlockContact(id, true),
		"remove": a.e.RemoveContact(id),
	} {
		if !errors.Is(err, ErrUnknownContact) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := a.e.SafetyNumber(id); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	if _, err := a.e.Contact(id); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	if _, err := a.e.SendText(id, "x"); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	if _, err := a.e.SendImage(context.Background(), id, "x.png", ""); !errors.Is(err, ErrUnknownContact) {
		t.Fatal(err)
	}
	if err := a.e.Disconnect(id); err != nil {
		t.Fatal(err)
	}
	a.e.SetTyping(id, true) // typing disabled: no-op
	if len(a.e.Queue(id)) != 0 {
		t.Fatal("queue")
	}
	var tid TransferID
	for name, err := range map[string]error{"accept": a.e.AcceptImage(tid, ""), "reject": a.e.RejectImage(tid), "cancel": a.e.CancelTransfer(tid)} {
		if !errors.Is(err, ErrUnknownTransfer) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := a.e.CreateInvite(InviteOptions{}); err == nil {
		t.Fatal("invite without host")
	}
	if _, err := a.e.CreateInvite(InviteOptions{Host: "bad host"}); err == nil {
		t.Fatal("invite with a bad host")
	}
	inv, err := a.e.CreateInvite(InviteOptions{Host: strings.Repeat("a", 56) + ".onion", Kind: transport.KindTor, TTL: time.Minute})
	if err != nil || !strings.HasPrefix(inv.String, "burrow1:") {
		t.Fatal(err)
	}
	if err := a.e.RevokeInvite("nope"); err == nil {
		t.Fatal("revoke unknown")
	}
	// A Tor invite without the Tor transport: no network action, clear error.
	if _, err := a.e.Connect(context.Background(), Target{Invite: inv.String}); !errors.Is(err, ErrNoTransport) {
		t.Fatal(err)
	}
	if _, err := a.e.Connect(context.Background(), Target{}); err == nil {
		t.Fatal("empty target")
	}
	if a.e.OnionAddress() != "" {
		t.Fatal("onion address without tor")
	}
	if got := a.e.Identity(); got.ID != a.id() || len(got.Fingerprint) != 52 {
		t.Fatal(got)
	}
	if err := a.e.RenameContact(id, string([]byte{0xff})); err == nil {
		t.Fatal("invalid utf8 nickname")
	}
}

func TestQueueLimitAndContactWithoutAddress(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	_ = b.e.Disconnect(a.id())
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	b.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	// A never dialed B: it has no address for B.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := a.e.Connect(ctx, Target{Contact: ptr(b.id())}); !errors.Is(err, ErrNoAddress) {
		t.Fatal(err)
	}
	for i := 0; i < wire.MaxQueuedMessages; i++ {
		if _, err := a.e.SendText(b.id(), "q"); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := a.e.SendText(b.id(), "overflow"); !errors.Is(err, ErrQueueFull) {
		t.Fatal(err)
	}
	if _, err := a.e.SendText(b.id(), strings.Repeat("x", wire.MaxTextBytes+1)); err == nil {
		t.Fatal("oversize text")
	}
	if len(a.e.Queue(b.id())) != wire.MaxQueuedMessages {
		t.Fatal("queue length")
	}
}

func TestCorruptStateBlobs(t *testing.T) {
	a := newNode(t, Config{})
	for blob, bad := range map[string]string{
		contactsBlob: `[{"id":"zz"}]`, invitesBlob: `[{"token":"12"}]`,
	} {
		if err := a.e.st.WriteBlob(blob, []byte(bad)); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{}, a.e.st, nil, nil); err == nil {
			t.Errorf("corrupt %s accepted", blob)
		}
		_ = a.e.st.DeleteBlob(blob)
	}
	if err := a.e.st.WriteBlob(contactsBlob, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{}, a.e.st, nil, nil); err == nil {
		t.Fatal("non-JSON contacts accepted")
	}
	_ = a.e.st.DeleteBlob(contactsBlob)
	// An expired invite is dropped on load.
	a.e.clockOffset.Store(int64(-2 * time.Hour))
	a.invite(t, false)
	a.e.clockOffset.Store(0)
	e2, err := New(Config{}, a.e.st, nil, nil)
	if err != nil || len(e2.Invites()) != 0 {
		t.Fatal(err)
	}
	e2.Close()
}

func TestPartialCleanupRules(t *testing.T) {
	a := newNode(t, Config{})
	b := newNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	dir := a.e.partialsPath()
	_ = os.MkdirAll(dir, 0o700)
	write := func(id string, peer PeerID) {
		tr := &transfer{peer: peer, size: 10}
		raw, _ := hex.DecodeString(id)
		copy(tr.partID[:], raw)
		if err := a.e.writeMeta(tr, 0); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(dir, id+".part"), []byte("x"), 0o600)
	}
	keep, gone := strings.Repeat("aa", 16), strings.Repeat("bb", 16)
	stranger, _ := identity.Generate()
	write(keep, b.id())
	write(gone, stranger.Public())
	_ = a.e.st.WriteBlob("partials/"+strings.Repeat("cc", 16)+".meta", []byte("garbage"))
	a.e.cleanPartials()
	if _, err := os.Stat(filepath.Join(dir, keep+".part")); err != nil {
		t.Fatal("valid partial removed")
	}
	if _, err := os.Stat(filepath.Join(dir, gone+".part")); err == nil {
		t.Fatal("partial of a removed contact kept")
	}
	if metas, _ := a.e.st.ListBlobs("partials"); len(metas) != 1 {
		t.Fatal(metas)
	}
	if n, err := a.e.Burn(); err != nil || n != 2 {
		t.Fatal(n, err)
	}
}

// A raw peer without the images feature, offers with bad dimensions, and
// accept/reject state errors.
func TestImageEdgeCases(t *testing.T) {
	a := imgNode(t, Config{})
	self, _ := identity.Generate()
	parsed := a.invite(t, false)
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", itoa(a.port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	inv, _ := parseInvite(parsed.String)
	p, err := testpeer.Dial(context.Background(), conn, self, a.id(), &inv)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Hello(wire.Hello{}); err != nil { // no images feature
		t.Fatal(err)
	}
	if in, err := p.Recv(3 * time.Second); err != nil || in.Type != wire.TypeHello {
		t.Fatal(err)
	}
	a.wait(t, "PeerConnected", isType[PeerConnected])
	path, _ := pngFixture(t, 8, 8, false)
	if _, err := a.e.SendImage(context.Background(), self.Public(), path, ""); !errors.Is(err, ErrPeerNoImages) {
		t.Fatal(err)
	}
	if _, err := a.e.SendImage(context.Background(), self.Public(), path, string([]byte{0xff})); err == nil {
		t.Fatal("invalid caption accepted")
	}
	// The raw peer offers an image with bomb dimensions: rejected as unsupported, no event.
	bomb, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: 100, Format: 1, Width: 20000, Height: 20000})
	if err := p.SendFrame(wire.TypeImgOffer, 2, bomb); err != nil {
		t.Fatal(err)
	}
	in, err := p.Recv(3 * time.Second)
	if err != nil || in.Type != wire.TypeImgReject {
		t.Fatal(in.Type, err)
	}
	if r, _ := wire.DecodeImgReject(in.Payload); r != wire.RejectUnsupported {
		t.Fatal(r)
	}
	// A sane offer, accepted twice (second is a state error), then the peer cancels.
	ok, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: 100, Format: 1, Width: 2, Height: 2, Caption: []byte("c")})
	if err := p.SendFrame(wire.TypeImgOffer, 4, ok); err != nil {
		t.Fatal(err)
	}
	off := a.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if err := a.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := a.e.AcceptImage(off.ID, ""); !errors.Is(err, ErrNotOffered) {
		t.Fatal(err)
	}
	if err := a.e.RejectImage(off.ID); !errors.Is(err, ErrNotOffered) {
		t.Fatal(err)
	}
	if in, err := p.Recv(3 * time.Second); err != nil || in.Type != wire.TypeImgAccept {
		t.Fatal(err)
	}
	// Wrong content for the announced hash → RESULT hash mismatch, partial removed.
	chunk, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: make([]byte, 100)})
	_ = p.SendFrame(wire.TypeImgChunk, 4, chunk)
	_ = p.SendFrame(wire.TypeImgDone, 4, nil)
	in, err = p.Recv(3 * time.Second)
	if err != nil || in.Type != wire.TypeImgResult {
		t.Fatal(in.Type, err)
	}
	if r, _ := wire.DecodeImgResult(in.Payload); r != wire.ResultHashMismatch {
		t.Fatal(r)
	}
	a.wait(t, "hash mismatch", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "hash mismatch" })
	if parts, _ := filepath.Glob(filepath.Join(a.e.partialsPath(), "*.part")); len(parts) != 0 {
		t.Fatal("bad partial kept")
	}
}

func parseInvite(s string) ([16]byte, error) {
	// The token is what the raw peer needs; reuse the engine's parser through Connect's path.
	inv, err := inviteParse(s)
	return inv, err
}
