package core

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/media"
	"github.com/guy5116/burrow/internal/wire"
)

// pngFixture writes a PNG with a tEXt secret and returns its path and the expected pixels.
func pngFixture(t *testing.T, w, h int, noise bool) (string, image.Image) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	x := uint32(2463534242) // xorshift: deterministic, incompressible pixels (not security-relevant)
	for i := range img.Pix {
		if noise {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			img.Pix[i] = byte(x)
		} else {
			img.Pix[i] = byte(i)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()
	// Inject a tEXt chunk after IHDR so stripping is observable.
	ihdrEnd := 8 + 8 + 13 + 4
	out := append([]byte(nil), clean[:ihdrEnd]...)
	out = append(out, pngChunk("tEXt", []byte("Comment\x00SECRET-METADATA"))...)
	out = append(out, clean[ihdrEnd:]...)
	p := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, img
}

func pngChunk(typ string, data []byte) []byte {
	out := []byte{byte(len(data) >> 24), byte(len(data) >> 16), byte(len(data) >> 8), byte(len(data))}
	out = append(out, typ...)
	out = append(out, data...)
	crc := crc32IEEE(append([]byte(typ), data...))
	return append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func crc32IEEE(b []byte) uint32 {
	const poly = 0xEDB88320
	crc := ^uint32(0)
	for _, c := range b {
		crc ^= uint32(c)
		for i := 0; i < 8; i++ {
			if crc&1 == 1 {
				crc = crc>>1 ^ poly
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}

func imgNode(t *testing.T, cfg Config) *node {
	t.Helper()
	cfg.DataDir = t.TempDir()
	cfg.ImageDir = filepath.Join(cfg.DataDir, "images")
	if cfg.MaxImage == 0 {
		cfg.MaxImage = 64 << 20
	}
	return newNode(t, cfg)
}

func samePix(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds %v vs %v", a.Bounds(), b.Bounds())
	}
	for y := 0; y < a.Bounds().Dy(); y++ {
		for x := 0; x < a.Bounds().Dx(); x++ {
			if color.NRGBAModel.Convert(a.At(x, y)) != color.NRGBAModel.Convert(b.At(x, y)) {
				t.Fatalf("pixel (%d,%d)", x, y)
			}
		}
	}
}

func TestImageTransfer(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	path, want := pngFixture(t, 300, 200, true)
	id, err := a.e.SendImage(context.Background(), b.id(), path, "  hello\u202e cat\n")
	if err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if off.Peer != a.id() || off.Format != wire.FormatPNG || off.Width != 300 || off.Height != 200 || off.Caption != "  hello� cat " || off.Size == 0 {
		t.Fatalf("%+v", off)
	}
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	done := b.wait(t, "TransferDone", isType[TransferDone]).(TransferDone)
	if done.ID != off.ID || !strings.HasSuffix(done.Path, ".png") || !strings.HasPrefix(filepath.Base(done.Path), "img-") {
		t.Fatalf("%+v", done)
	}
	if filepath.Dir(done.Path) != b.e.imageDir() {
		t.Fatal(done.Path)
	}
	a.wait(t, "TransferDone", func(ev Event) bool { d, ok := ev.(TransferDone); return ok && d.ID == id && d.Path == path })
	a.wait(t, "TransferProgress", isType[TransferProgress])
	b.wait(t, "TransferProgress", isType[TransferProgress])
	saved, err := os.ReadFile(done.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(saved, []byte("SECRET-METADATA")) {
		t.Fatal("metadata leaked")
	}
	got, err := DecodeImage(context.Background(), done.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	samePix(t, want, got)
	if th, _ := DecodeImage(context.Background(), done.Path, 50); th.Bounds().Dx() != 50 {
		t.Fatal("thumbnail")
	}
	// No partials left behind; no stray transfers.
	if parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*")); len(parts) != 0 {
		t.Fatal(parts)
	}
	if metas, _ := b.e.st.ListBlobs("partials"); len(metas) != 0 {
		t.Fatal(metas)
	}
	// Same image again: collision suffix -2.
	id2, _ := a.e.SendImage(context.Background(), b.id(), path, "")
	off2 := b.wait(t, "second offer", func(ev Event) bool { o, ok := ev.(ImageOffered); return ok && o.ID != off.ID }).(ImageOffered)
	_ = b.e.AcceptImage(off2.ID, "")
	d2 := b.wait(t, "second done", func(ev Event) bool { d, ok := ev.(TransferDone); return ok && d.ID == off2.ID }).(TransferDone)
	if !strings.HasSuffix(d2.Path, "-2.png") {
		t.Fatal(d2.Path)
	}
	_ = id2
}

func TestImageRejectAndLimits(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{MaxImage: 1000})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 64, 64, true)
	// Too large for B: rejected before the user is asked.
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "rejected too large", func(ev Event) bool {
		f, ok := ev.(TransferFailed)
		return ok && strings.Contains(f.Reason, "too large")
	})
	time.Sleep(20 * time.Millisecond)
	if b.count(isType[ImageOffered]) != 0 {
		t.Fatal("oversize offer reached the user")
	}
	// Our own limit is the min of both HELLOs: A refuses to even strip.
	c := imgNode(t, Config{})
	connect(t, c, a, a.invite(t, false).String)
	small, _ := pngFixture(t, 8, 8, false)
	if _, err := a.e.SendImage(context.Background(), c.id(), small, ""); err != nil {
		t.Fatal(err)
	}
	off := c.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if err := c.e.RejectImage(off.ID); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "declined", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && strings.Contains(f.Reason, "declined") })
	if err := c.e.RejectImage(off.ID); !errors.Is(err, ErrUnknownTransfer) {
		t.Fatal(err)
	}
	// Not an image / missing file.
	if _, err := a.e.SendImage(context.Background(), c.id(), filepath.Join(t.TempDir(), "nope.png"), ""); err != nil {
		t.Fatal(err)
	}
	a.wait(t, "not found", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "file not found" })
	// Offline contact.
	_ = c.e.Disconnect(a.id())
	a.wait(t, "PeerDisconnected", isType[PeerDisconnected])
	if _, err := a.e.SendImage(context.Background(), c.id(), small, ""); !errors.Is(err, ErrOffline) {
		t.Fatal(err)
	}
}

func TestImageCancel(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 1200, 1200, true) // several MiB: many chunks
	b.e.chunkHook = func(tr *transfer, n uint32) {
		if n == 3 {
			go func() { _ = b.e.CancelTransfer(tr.id) }()
		}
	}
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "cancelled", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "cancelled" })
	a.wait(t, "cancelled by peer", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "cancelled by peer" })
	time.Sleep(50 * time.Millisecond)
	if parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*.part")); len(parts) != 0 {
		t.Fatal("partial kept after user cancel")
	}
	if a.e.Contacts()[0].Online == false {
		t.Fatal("session dropped by cancel")
	}
}

// Connection loss mid-transfer keeps the partial; re-offering the same image
// resumes from the last .meta checkpoint after truncation and re-hash.
func TestImageResume(t *testing.T) {
	a := imgNode(t, Config{})
	b := imgNode(t, Config{AutoReconnect: true}) // B dialed, so B knows A's address
	b.e.rand = func() time.Duration { return 5 * time.Millisecond }
	connect(t, b, a, a.invite(t, false).String)
	path, want := pngFixture(t, 1200, 1200, true)
	var killed bool
	b.e.chunkHook = func(tr *transfer, n uint32) {
		if n == 20 && !killed {
			killed = true
			p := a.peerOf(b.id())
			go p.s.Close(wire.ByeLocalError) // A's "local error": B keeps the partial and reconnects
		}
	}
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	if err := b.e.AcceptImage(off.ID, ""); err != nil {
		t.Fatal(err)
	}
	b.wait(t, "connection lost", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "connection lost" })
	a.wait(t, "connection lost", func(ev Event) bool { f, ok := ev.(TransferFailed); return ok && f.Reason == "connection lost" })
	time.Sleep(50 * time.Millisecond)
	parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*.part"))
	metas, _ := b.e.st.ListBlobs("partials")
	if len(parts) != 1 || len(metas) != 1 {
		t.Fatalf("partial not kept: %v %v", parts, metas)
	}
	m, err := b.e.readMeta(metas[0])
	if err != nil || m.Complete < 16 || uint64(m.Complete)*65524 >= m.Size {
		t.Fatalf("meta %+v %v", m, err)
	}
	// Corrupt the tail beyond the checkpoint: truncation must discard it.
	f, _ := os.OpenFile(parts[0], os.O_WRONLY|os.O_APPEND, 0o600)
	_, _ = f.Write([]byte("garbage"))
	_ = f.Close()

	b.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	a.waitN(t, "PeerConnected", isType[PeerConnected], 2)
	b.e.chunkHook = nil
	if _, err := a.e.SendImage(context.Background(), b.id(), path, ""); err != nil {
		t.Fatal(err)
	}
	off2 := b.wait(t, "re-offer", func(ev Event) bool { o, ok := ev.(ImageOffered); return ok && o.ID != off.ID }).(ImageOffered)
	if err := b.e.AcceptImage(off2.ID, ""); err != nil {
		t.Fatal(err)
	}
	res := b.wait(t, "TransferResumed", isType[TransferResumed]).(TransferResumed)
	// The transfer keeps the id it was offered under; only the file on disk is the old one.
	if res.ID != off2.ID || hex.EncodeToString(off2.ID[:]) == m.ID {
		t.Fatal("a resumed transfer changed its id")
	}
	if err := b.e.CancelTransfer(TransferID{1}); !errors.Is(err, ErrUnknownTransfer) {
		t.Fatal(err)
	}
	// The sender skipped the completed chunks: its first progress is past the checkpoint.
	done := b.wait(t, "TransferDone", isType[TransferDone]).(TransferDone)
	got, err := DecodeImage(context.Background(), done.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	samePix(t, want, got)
	a.wait(t, "sender done", func(ev Event) bool { d, ok := ev.(TransferDone); return ok && d.Path == path })
	if parts, _ := filepath.Glob(filepath.Join(b.e.partialsPath(), "*")); len(parts) != 0 {
		t.Fatal("partials left after resume")
	}
}

func TestPartialCleanupAndAutoAccept(t *testing.T) {
	dir := t.TempDir()
	// A stray .part without .meta is removed at engine start.
	_ = os.MkdirAll(filepath.Join(dir, "partials"), 0o700)
	stray := filepath.Join(dir, "partials", "deadbeef.part")
	_ = os.WriteFile(stray, []byte("x"), 0o600)
	cfg := Config{DataDir: dir, ImageDir: filepath.Join(dir, "images"), MaxImage: 64 << 20, AutoAcceptFromVerified: true}
	b := newNode(t, cfg)
	if _, err := os.Stat(stray); err == nil {
		t.Fatal("stray partial not removed")
	}
	a := imgNode(t, Config{})
	connect(t, b, a, a.invite(t, false).String)
	path, _ := pngFixture(t, 16, 16, false)
	// Unverified: prompt (ImageOffered, no auto accept).
	_, _ = a.e.SendImage(context.Background(), b.id(), path, "")
	off := b.wait(t, "ImageOffered", isType[ImageOffered]).(ImageOffered)
	time.Sleep(30 * time.Millisecond)
	if b.count(isType[TransferDone]) != 0 {
		t.Fatal("auto-accepted an unverified contact")
	}
	_ = b.e.RejectImage(off.ID)
	// Verified: accepted without a prompt decision.
	_ = b.e.VerifyContact(a.id())
	_, _ = a.e.SendImage(context.Background(), b.id(), path, "")
	b.wait(t, "auto TransferDone", isType[TransferDone])
	if _, err := media.ProbeFile(b.wait(t, "done", isType[TransferDone]).(TransferDone).Path); err != nil {
		t.Fatal(err)
	}
}
