package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/invite"
	"github.com/guy5116/burrow/internal/media"
	"github.com/guy5116/burrow/internal/testpeer"
	"github.com/guy5116/burrow/internal/wire"
)

// fileFixture writes size random bytes that are not an image.
func fileFixture(t *testing.T, name string, size int) (string, []byte) {
	t.Helper()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	copy(data, "PK\x03\x04")
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func isFileOffer(ev Event) bool { o, ok := ev.(ImageOffered); return ok && o.Format == FormatFile }

func TestFileTransfer(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	path, want := fileFixture(t, "Quarterly Report (final).ZIP", 3*wire.ChunkData+17)
	id, err := a.e.SendFile(context.Background(), b.id(), path, "the archive", false)
	if err != nil {
		t.Fatal(err)
	}
	// The size is known before anything is downloaded; the name never travels.
	off := b.wait(t, "file offer", isFileOffer).(ImageOffered)
	if off.Size != uint64(len(want)) || off.Ext != "zip" || off.Caption != "the archive" || off.Width != 0 {
		t.Fatalf("%+v", off)
	}
	if parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*")); len(parts) != 0 {
		t.Fatal("bytes on disk before the offer was accepted")
	}
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	done := b.wait(t, "TransferDone", isType[TransferDone]).(TransferDone)
	base := filepath.Base(done.Path)
	if done.Image || !strings.HasPrefix(base, "file-") || !strings.HasSuffix(base, ".zip") || strings.Contains(base, "Report") {
		t.Fatalf("%+v", done)
	}
	got, err := os.ReadFile(done.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("received file differs", err)
	}
	a.wait(t, "sender done", func(ev Event) bool { d, ok := ev.(TransferDone); return ok && d.ID == id && !d.Image })
	// A received file of another kind is never decoded or read into memory for display.
	if _, err := DecodeImage(context.Background(), done.Path, 0); !errors.Is(err, media.ErrUnsupported) {
		t.Fatal(err)
	}
	// No extension: saved as .bin.
	path2, _ := fileFixture(t, "LICENSE", 100)
	if _, err := a.e.SendFile(context.Background(), b.id(), path2, "", false); err != nil {
		t.Fatal(err)
	}
	off2 := b.wait(t, "second offer", func(ev Event) bool { o, ok := ev.(ImageOffered); return ok && o.ID != off.ID }).(ImageOffered)
	if off2.Ext != "" || off2.Size != 100 {
		t.Fatalf("%+v", off2)
	}
	_ = b.e.AcceptImage(off2.ID, "")
	d2 := b.wait(t, "second done", func(ev Event) bool { d, ok := ev.(TransferDone); return ok && d.ID == off2.ID }).(TransferDone)
	if !strings.HasSuffix(d2.Path, ".bin") {
		t.Fatal(d2.Path)
	}
}

// An image given to SendFile still loses its metadata.
func TestSendFileStripsImages(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 40, 30, false)
	if _, err := a.e.SendFile(context.Background(), b.id(), path, "", false); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if off.Format != wire.FormatPNG || off.Width != 40 {
		t.Fatalf("%+v", off)
	}
	_ = b.e.AcceptImage(off.ID, "")
	done := b.wait(t, "TransferDone", isType[TransferDone]).(TransferDone)
	saved, _ := os.ReadFile(done.Path)
	if !done.Image || len(saved) == 0 || bytes.Contains(saved, []byte("SECRET-METADATA")) {
		t.Fatal("image sent as a file kept its metadata")
	}
}

func TestFileSizeLimits(t *testing.T) {
	a := imgNode(t, Config{MaxFile: 1 << 20})
	b := imgNode(t, Config{MaxFile: 4096}) // b accepts at most 4 KiB
	c := imgNode(t, Config{NoFiles: true})
	connect(t, b, a, a.invite(t, false).String)
	connect(t, c, a, a.invite(t, false).String)
	big, _ := fileFixture(t, "big.rar", 4097)
	ok, _ := fileFixture(t, "ok.rar", 4096)

	// Over the receiver's advertised limit: refused locally, nothing is offered.
	id, err := a.e.SendFile(context.Background(), b.id(), big, "", false)
	if err != nil {
		t.Fatal(err)
	}
	f := a.wait(t, "TransferFailed", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.ID == id }).(TransferFailed)
	if !strings.HasPrefix(f.Reason, "too large") {
		t.Fatal(f.Reason)
	}
	if _, err := a.e.SendFile(context.Background(), b.id(), ok, "", false); err != nil {
		t.Fatal(err)
	}
	if off := b.wait(t, "file offer", isFileOffer).(ImageOffered); off.Size != 4096 {
		t.Fatalf("%+v", off)
	}
	if b.count(isFileOffer) != 1 {
		t.Fatal("the oversized file was offered")
	}
	// A contact who turned files off cannot be offered one, and offers none.
	if _, err := a.e.SendFile(context.Background(), c.id(), ok, "", false); !errors.Is(err, ErrPeerNoFiles) {
		t.Fatal(err)
	}
	if _, err := c.e.SendFile(context.Background(), a.id(), ok, "", false); err == nil {
		t.Fatal("sent a file with files turned off")
	}
	// Images still work with files turned off.
	img, _ := pngFixture(t, 10, 10, false)
	if _, err := a.e.SendFile(context.Background(), c.id(), img, "", false); err != nil {
		t.Fatal(err)
	}
	c.wait(t, "ImageOffered", isType[ImageOffered])
}

// A peer that ignores our advertised limit is refused before the user is asked.
func TestOversizedFileOfferRejected(t *testing.T) {
	a := imgNode(t, Config{MaxFile: 1000})
	p := rawPeer(t, a, wire.Hello{Features: wire.FeatureFiles, MaxFile: 1 << 30})
	offer, _ := wire.AppendFileOffer(nil, wire.FileOffer{Size: 1001, Ext: []byte("iso")})
	if err := p.SendFrame(wire.TypeFileOffer, 2, offer); err != nil {
		t.Fatal(err)
	}
	in := recvType(t, p, wire.TypeImgReject)
	if r, _ := wire.DecodeImgReject(in.Payload); r != wire.RejectTooLarge {
		t.Fatal(r)
	}
	offer, _ = wire.AppendFileOffer(nil, wire.FileOffer{Size: 1000, Ext: []byte("iso")})
	if err := p.SendFrame(wire.TypeFileOffer, 4, offer); err != nil {
		t.Fatal(err)
	}
	if off := a.wait(t, "file offer", isFileOffer).(ImageOffered); off.Size != 1000 || off.Ext != "iso" {
		t.Fatalf("%+v", off)
	}
	if a.count(isFileOffer) != 1 {
		t.Fatal("the oversized offer reached the user")
	}
}

func recvType(t *testing.T, p *testpeer.Peer, typ wire.FrameType) wire.Inner {
	t.Helper()
	for {
		in, err := p.Recv(5 * time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if in.Type == typ {
			return in
		}
	}
}

// rawPeer connects a scriptable peer to a with the given HELLO.
func rawPeer(t *testing.T, a *node, h wire.Hello) *testpeer.Peer {
	t.Helper()
	self, _ := identity.Generate()
	parsed, _ := invite.Parse(a.invite(t, false).String)
	p, _ := rawDial(t, a, self, &parsed.Token)
	if err := p.Hello(h); err != nil {
		t.Fatal(err)
	}
	recvType(t, p, wire.TypeHello)
	return p
}

// Auto-accept is for images from verified contacts; a file always waits for the user.
func TestAutoAcceptNeverAppliesToFiles(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{AutoAcceptFromVerified: true})
	connect(t, b, a, a.invite(t, false).String)
	if err := b.e.VerifyContact(a.id()); err != nil {
		t.Fatal(err)
	}
	path, _ := fileFixture(t, "x.pdf", 5000)
	if _, err := a.e.SendFile(context.Background(), b.id(), path, "", false); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "file offer", isFileOffer).(ImageOffered)
	img, _ := pngFixture(t, 10, 10, false)
	if _, err := a.e.SendFile(context.Background(), b.id(), img, "", false); err != nil {
		t.Fatal(err)
	}
	done := b.wait(t, "image auto-accepted", isType[TransferDone]).(TransferDone)
	if !done.Image || done.ID == off.ID {
		t.Fatalf("%+v", done)
	}
	if err := b.e.RejectImage(off.ID); err != nil { // still waiting for a decision
		t.Fatal(err)
	}
}
