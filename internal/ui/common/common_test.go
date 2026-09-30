package common

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
)

var fast = store.Options{Argon2: store.Argon2{Time: 1, MemKiB: store.MinMemKiB, Threads: 1}}

type node struct {
	e   *core.Engine
	ctl *Controller
	ev  chan core.Event
}

func newNode(t *testing.T, cfg core.Config) *node {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Init(dir, []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ListenAddr, cfg.DataDir = "127.0.0.1:0", dir
	e, err := core.New(cfg, st, core.Transports("127.0.0.1:0", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Start(ctx); close(done) }()
	for len(e.ListenAddrs()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	n := &node{e: e, ctl: NewController(e, "127.0.0.1"), ev: make(chan core.Event, 1024)}
	go func() {
		for {
			select {
			case ev := <-e.Events():
				n.ctl.Observe(ev)
				n.ev <- ev
			case <-e.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	return n
}

func (n *node) wait(t *testing.T, pred func(core.Event) bool) core.Event {
	t.Helper()
	deadline := time.After(8 * time.Second)
	for {
		select {
		case ev := <-n.ev:
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timeout waiting for event")
		}
	}
}

func exec(t *testing.T, c *Controller, line string) string {
	t.Helper()
	out, _ := c.Exec(context.Background(), line)
	return strings.Join(out, "\n")
}

func TestControllerCommands(t *testing.T) {
	a := newNode(t, core.Config{DisplayName: "Alice"})
	b := newNode(t, core.Config{DisplayName: "Bob"})
	// Nothing selected yet.
	for _, line := range []string{"hello", "/image x.png", "/history"} {
		if out := exec(t, b.ctl, line); !strings.HasPrefix(out, "! no conversation") {
			t.Errorf("%q → %q", line, out)
		}
	}
	if out := exec(t, b.ctl, "/contacts"); out != "(no contacts)" {
		t.Fatal(out)
	}
	if out, quit := b.ctl.Exec(context.Background(), ""); out != nil || quit {
		t.Fatal("blank line")
	}
	if out := exec(t, b.ctl, "/help"); !strings.Contains(out, "/safety") || !strings.Contains(out, "/image") {
		t.Fatal(out)
	}
	if out := exec(t, b.ctl, "/id"); !strings.Contains(out, b.e.Identity().Display) {
		t.Fatal(out)
	}
	b.ctl.Onion = "abc.onion"
	if out := exec(t, b.ctl, "/id"); !strings.Contains(out, "abc.onion") {
		t.Fatal(out)
	}
	b.ctl.Onion = ""
	if out := exec(t, b.ctl, "/invite tor"); !strings.HasPrefix(out, "! tor is not running") {
		t.Fatal(out)
	}
	if out := exec(t, b.ctl, "/nope"); !strings.Contains(out, "unknown command") {
		t.Fatal(out)
	}
	if out := exec(t, b.ctl, "/connect"); !strings.HasPrefix(out, "! usage") {
		t.Fatal(out)
	}
	if out := exec(t, b.ctl, "/connect burrow1:bogus"); !strings.HasPrefix(out, "! ") {
		t.Fatal(out)
	}
	if out := exec(t, b.ctl, "/to nobody"); !strings.HasPrefix(out, "! ") {
		t.Fatal(out)
	}

	// Invite on A (port from the bound listener), connect from B.
	port := a.e.ListenAddrs()[0].(*net.TCPAddr).Port
	inv := exec(t, a.ctl, "/invite multi 127.0.0.1")
	lines := strings.Split(inv, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "burrow1:") || !strings.Contains(lines[0], "multi-use") || port == 0 {
		t.Fatal(inv)
	}
	if out := exec(t, b.ctl, "/connect "+lines[1]); !strings.Contains(out, "connected to") || !isSelected(b.ctl) {
		t.Fatal(out)
	}
	a.wait(t, func(ev core.Event) bool { _, ok := ev.(core.PeerConnected); return ok })
	aFP, bFP := a.e.Identity().Fingerprint, b.e.Identity().Fingerprint

	// Resolve by nickname, by prefix, by full fingerprint; ambiguous and short prefixes fail.
	for _, name := range []string{"Alice", aFP[:10], aFP, a.e.Identity().Display} {
		if id, err := b.ctl.Resolve(name); err != nil || id != a.e.Identity().ID {
			t.Errorf("Resolve(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", aFP[:4], "alice"} {
		if _, err := b.ctl.Resolve(name); err == nil {
			t.Errorf("Resolve(%q) succeeded", name)
		}
	}

	// Messaging.
	if out := exec(t, b.ctl, "hi there"); !strings.Contains(out, "queued") {
		t.Fatal(out)
	}
	got := a.wait(t, func(ev core.Event) bool { _, ok := ev.(core.MessageReceived); return ok }).(core.MessageReceived)
	if got.Text != "hi there" {
		t.Fatal(got)
	}
	if out := exec(t, a.ctl, "/msg "+bFP[:12]+" reply"); !strings.Contains(out, "queued") {
		t.Fatal(out)
	}
	b.wait(t, func(ev core.Event) bool { m, ok := ev.(core.MessageReceived); return ok && m.Text == "reply" })
	if out := exec(t, a.ctl, "/msg nobody x"); !strings.HasPrefix(out, "! ") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/to "+bFP[:12]); !strings.Contains(out, "now talking to") {
		t.Fatal(out)
	}

	// Contacts, safety, verify, rename, block, unblock.
	if out := exec(t, a.ctl, "/contacts"); !strings.Contains(out, "Bob") || !strings.Contains(out, "unverified") || !strings.HasPrefix(out, "*") {
		t.Fatal(out)
	}
	sa, sb := exec(t, a.ctl, "/safety Bob"), exec(t, b.ctl, "/safety Alice")
	if strings.SplitN(sa, "\n", 2)[1] != strings.SplitN(sb, "\n", 2)[1] || len(strings.Split(sa, "\n")) != 4 {
		t.Fatalf("%q vs %q", sa, sb)
	}
	if out := exec(t, a.ctl, "/verify Bob"); !strings.Contains(out, "verified") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/contacts"); !strings.HasSuffix(out, "  verified") {
		t.Fatal(out)
	}
	if label := a.ctl.Names.Label(b.e.Identity().ID); label != "Bob ("+b.e.Identity().ID.Short()+", verified)" {
		t.Fatal(label)
	}
	if out := exec(t, a.ctl, "/rename Bob Robert"); !strings.Contains(out, "Robert") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/rename Robert "); !strings.HasPrefix(out, "! ") {
		t.Fatal(out)
	}
	for _, cmd := range []string{"/safety", "/verify", "/rename", "/block", "/remove", "/disconnect"} {
		if out := exec(t, a.ctl, cmd+" nobody"); !strings.HasPrefix(out, "! ") {
			t.Errorf("%s nobody → %q", cmd, out)
		}
	}
	// Typing indicators are off in the settings of this node: saying "on" would be a lie.
	if out := exec(t, a.ctl, "/typing on"); !strings.HasPrefix(out, "! ") || a.ctl.TypingOn() {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/typing sometimes"); !strings.HasPrefix(out, "! usage") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/typing off"); !strings.Contains(out, "off") || a.ctl.TypingOn() {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/history 5"); !strings.Contains(out, "history is off") {
		t.Fatal(out)
	}

	// Transfers by number.
	if out := exec(t, a.ctl, "/transfers"); out != "(no transfers)" {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/images"); out != "(no images received yet)" {
		t.Fatal(out)
	}
	for _, cmd := range []string{"/accept x", "/accept 9", "/reject 9", "/cancel 9"} {
		if out := exec(t, a.ctl, cmd); !strings.HasPrefix(out, "! ") {
			t.Errorf("%s → %q", cmd, out)
		}
	}
	if out := exec(t, a.ctl, "/image"); !strings.HasPrefix(out, "! usage") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/view 1"); !strings.HasPrefix(out, "! /view") {
		t.Fatal(out)
	}
	var id core.TransferID
	id[0] = 7
	a.ctl.Observe(core.ImageOffered{ID: id, Size: 5 << 30, Ext: "zip"})
	if out := exec(t, a.ctl, "/transfers"); a.ctl.Number(id) != 1 || !strings.Contains(out, "#1 a file: 5.00 GiB, type .zip") {
		t.Fatal("offer numbering", out)
	}
	if out := exec(t, a.ctl, "/file"); !strings.HasPrefix(out, "! usage: /file") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/file /nonexistent/x.zip"); !strings.HasPrefix(out, "! open") { // the file is read before anything is offered
		t.Fatal(out)
	}
	// A received file that is not an image is never listed for display.
	a.ctl.Observe(core.TransferDone{ID: id, Path: "/x/file-1.zip"})
	if len(a.ctl.Images()) != 0 {
		t.Fatal("a file was listed as an image")
	}
	a.ctl.Observe(core.ImageOffered{ID: id})
	a.ctl.Observe(core.TransferDone{ID: id, Path: "/x/img.png", Image: true})
	if a.ctl.Number(id) != 0 || len(a.ctl.Images()) != 1 || !strings.Contains(exec(t, a.ctl, "/images"), "/x/img.png") {
		t.Fatal("done bookkeeping")
	}
	a.ctl.Observe(core.ImageOffered{ID: id})
	a.ctl.Observe(core.TransferFailed{ID: id})
	if a.ctl.Number(id) != 0 {
		t.Fatal("failed bookkeeping")
	}

	// Block / unblock / disconnect / remove / quit.
	if out := exec(t, a.ctl, "/block Robert"); !strings.Contains(out, "blocked") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/contacts"); !strings.Contains(out, "BLOCKED") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/unblock Robert"); !strings.Contains(out, "unblocked") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/disconnect Robert"); !strings.Contains(out, "disconnecting") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/remove Robert"); !strings.Contains(out, "removed") || isSelected(a.ctl) {
		t.Fatal(out)
	}
	if _, quit := a.ctl.Exec(context.Background(), "/quit"); !quit {
		t.Fatal("quit")
	}
}

func TestLinesAndJSONForEveryEvent(t *testing.T) {
	n := newNode(t, core.Config{})
	names := n.ctl.Names
	var p core.PeerID
	p[0] = 1
	tp := core.TransferPeer{Peer: p}
	now := time.Unix(1_700_000_000, 0)
	events := []core.Event{
		core.PeerConnected{Peer: p, Transport: "tcp", Name: "x"}, core.PeerDisconnected{Peer: p, Reason: "closed"},
		core.Reconnecting{Peer: p, Attempt: 2, NextAt: now}, core.HandshakeFailed{Stage: "msg2", Reason: "r"},
		core.HandshakeFailed{Peer: p, Stage: "hello", Reason: "r"}, core.NewPeerViaInvite{Peer: p, InviteID: "i"},
		core.InviteConsumed{ID: "i"}, core.NameCollision{New: p, Existing: p, Name: "n"}, core.PeerVerified{Peer: p},
		core.MessageReceived{Peer: p, ID: 5, Text: "t", SentAt: now}, core.MessageReceived{Peer: p, ID: 6, Text: "t"},
		core.MessageStatus{Peer: p, ID: 5, Status: core.StatusDelivered}, core.Typing{Peer: p, Typing: true},
		core.ImageOffered{Peer: p, Size: 2 << 20, Format: 2, Width: 1, Height: 2, Caption: "c"}, core.ImageOffered{Peer: p, Size: 10},
		core.TransferProgress{Peer: tp, Done: 1, Size: 2}, core.TransferResumed{},
		core.TransferDone{Peer: tp, Path: "/p"}, core.TransferDone{Peer: core.TransferPeer{Peer: p, Outgoing: true}},
		core.TransferFailed{Peer: tp, Reason: "x"}, core.TransferFailed{Peer: core.TransferPeer{Peer: p, Outgoing: true}, Reason: "x"},
		core.ErrorEvent{Message: "m"},
	}
	for _, ev := range events {
		m := names.JSON(ev)
		if m["event"] == "" || strings.Contains(m["event"].(string), ".") {
			t.Errorf("%T: event name %v", ev, m["event"])
		}
		line := names.Line(ev)
		if _, quiet := ev.(core.TransferProgress); !quiet && line == "" {
			t.Errorf("%T: empty line", ev)
		}
	}
	if names.Line(core.Typing{Peer: p}) != "" {
		t.Fatal("typing stop should be silent")
	}
	if names.Label(p) != p.Short() || names.Nick(p) != p.Short() {
		t.Fatal("unknown peer label")
	}
	for in, want := range map[uint64]string{10: "10 B", 2048: "2 KiB", 3 << 20: "3.0 MiB"} {
		if Size(in) != want {
			t.Errorf("Size(%d)=%q", in, Size(in))
		}
	}
	for f, want := range map[uint8]string{1: "PNG", 2: "JPEG", 3: "WebP", 4: "GIF", 9: "image"} {
		if FormatName(f) != want {
			t.Errorf("FormatName(%d)", f)
		}
	}
	var sn core.SafetyNumber
	if got := FormatSafety(sn); strings.Count(got, "\n") != 2 || strings.Count(got, "00000") != 12 {
		t.Fatal(got)
	}
}

// The size comes first in every offer, so a huge file is obvious before accepting.
func TestOfferShowsSizeFirst(t *testing.T) {
	for _, c := range []struct {
		o    core.ImageOffered
		want string
	}{
		{core.ImageOffered{Size: 7 << 30, Ext: "rar"}, "a file: 7.00 GiB, type .rar"},
		{core.ImageOffered{Size: 1536 << 10, Ext: "pdf"}, "a file: 1.5 MiB, type .pdf"},
		{core.ImageOffered{Size: 10}, "a file: 10 B, no file type given"},
		{core.ImageOffered{Size: 2 << 20, Format: 2, Width: 640, Height: 480}, "an image: 2.0 MiB, JPEG 640×480"},
	} {
		if got := Offer(c.o); got != c.want {
			t.Errorf("%q want %q", got, c.want)
		}
	}
	n := newNode(t, core.Config{}).ctl.Names
	line := n.Line(core.ImageOffered{Size: 7 << 30, Ext: "rar", Caption: "films"})
	if !strings.Contains(line, "7.00 GiB") || !strings.Contains(line, "/accept") {
		t.Fatal(line)
	}
	m := n.JSON(core.ImageOffered{Size: 7 << 30, Ext: "rar"})
	if m["size"] != uint64(7<<30) || m["format"] != "file" || m["ext"] != "rar" {
		t.Fatal(m)
	}
	if m := n.JSON(core.TransferDone{Path: "/p"}); m["image"] != false {
		t.Fatal(m)
	}
}

func isSelected(c *Controller) bool {
	_, ok := c.Selected()
	return ok
}

func TestTerminalImages(t *testing.T) {
	for _, k := range []string{"KITTY_WINDOW_ID", "TERM", "TERM_PROGRAM"} {
		t.Setenv(k, "")
	}
	if DetectTerminal() != TermNone {
		t.Fatal("none")
	}
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	if DetectTerminal() != TermITerm2 {
		t.Fatal("iterm")
	}
	t.Setenv("TERM", "xterm-kitty")
	if DetectTerminal() != TermKitty {
		t.Fatal("kitty")
	}
	img := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	x := uint32(2463534242) // incompressible pixels so the PNG spans several 4096-byte chunks
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	var k, i2 bytes.Buffer
	if err := WriteImage(&k, img, TermKitty); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(k.Bytes(), []byte("\x1b_Gf=100,a=T,t=d,m=1;")) || !bytes.Contains(k.Bytes(), []byte("\x1b_Gm=0;")) {
		t.Fatalf("kitty framing: %q…", k.Bytes()[:40])
	}
	if err := WriteImage(&i2, img, TermITerm2); err != nil || !bytes.HasPrefix(i2.Bytes(), []byte("\x1b]1337;File=inline=1;size=")) {
		t.Fatal(err)
	}
	if err := WriteImage(&k, img, TermNone); err == nil {
		t.Fatal("no protocol must fail")
	}
}

func TestNewControllerFeatures(t *testing.T) {
	a := newNode(t, core.Config{DisplayName: "Alice"})
	b := newNode(t, core.Config{DisplayName: "Bob"})
	port := uint16(a.e.ListenAddrs()[0].(*net.TCPAddr).Port)

	// QR only when asked for, and only when the UI supplied a renderer.
	if out := exec(t, a.ctl, "/invite qr 127.0.0.1"); !strings.Contains(out, "QR codes are not available") {
		t.Fatal(out)
	}
	a.ctl.QR = func(s string) string { return "QR<" + s[:8] + ">\nrow2\n" }
	out := exec(t, a.ctl, "/invite qr 127.0.0.1")
	if lines := strings.Split(out, "\n"); len(lines) != 4 || lines[2] != "QR<burrow1:>" || lines[3] != "row2" {
		t.Fatalf("%q", out)
	}
	if out := exec(t, a.ctl, "/invite 127.0.0.1"); strings.Contains(out, "QR<") {
		t.Fatal("QR printed without being asked")
	}

	// A hostname invite prints the DNS note before connecting; an IP invite does not.
	inv, err := a.e.CreateInvite(core.InviteOptions{Host: "localhost", Port: port})
	if err != nil {
		t.Fatal(err)
	}
	out = exec(t, b.ctl, "/connect "+inv.String)
	if !strings.Contains(out, "note: this invite uses the hostname localhost") || !strings.Contains(out, "DNS resolver") {
		t.Fatal(out)
	}
	if !strings.Contains(out, "connected to") {
		t.Skipf("localhost did not resolve to the listener here: %q", out)
	}
	a.wait(t, func(ev core.Event) bool { _, ok := ev.(core.PeerConnected); return ok })
	ipInv, _ := a.e.CreateInvite(core.InviteOptions{Host: "127.0.0.1", Port: port})
	if info, err := core.DescribeInvite(ipInv.String); err != nil || info.Hostname {
		t.Fatal(info, err)
	}

	// The status line names the transport while online.
	if st := b.ctl.Status(); !strings.Contains(st, "online via tcp") || !strings.Contains(st, "UNVERIFIED") || !strings.Contains(st, "Alice") {
		t.Fatal(st)
	}
	_ = exec(t, b.ctl, "/verify Alice")
	_ = exec(t, b.ctl, "/disconnect Alice")
	b.wait(t, func(ev core.Event) bool { _, ok := ev.(core.PeerDisconnected); return ok })
	if st := b.ctl.Status(); !strings.Contains(st, "offline") || !strings.Contains(st, "verified") || strings.Contains(st, "via") {
		t.Fatal(st)
	}
	b.ctl.Deselect()
	if st := b.ctl.Status(); st != "me "+b.e.Identity().ID.Short() {
		t.Fatal(st)
	}
	var gone core.PeerID
	b.ctl.Select(gone)
	if st := b.ctl.Status(); strings.Contains(st, "talking") {
		t.Fatal(st)
	}

	// Partials: quiet at startup when there are none, explicit on request.
	if notes := b.ctl.StartupNotes(false); notes != nil {
		t.Fatal(notes)
	}
	if out := exec(t, b.ctl, "/partials"); out != "(no partial downloads)" {
		t.Fatal(out)
	}
}

func TestKittyInline(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 320, 160))
	in, err := KittyInline(img, 7, 32)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in.Transmit, "\x1b_Ga=T,U=1,f=100,q=2,i=7,c=32,r=8,m=0;") || !strings.HasSuffix(in.Transmit, "\x1b\\") {
		t.Fatalf("%q", in.Transmit[:60])
	}
	if len(in.Lines) != 8 {
		t.Fatal(len(in.Lines))
	}
	for r, line := range in.Lines {
		if !strings.HasPrefix(line, "\x1b[38;5;7m") || !strings.HasSuffix(line, "\x1b[39m") {
			t.Fatalf("row %d colour framing: %q", r, line)
		}
		body := strings.TrimSuffix(strings.TrimPrefix(line, "\x1b[38;5;7m"), "\x1b[39m")
		runes := []rune(body)
		if len(runes) != 33 || runes[0] != kittyPlaceholder || runes[1] != rowDiacritics[r] || runes[2] != kittyPlaceholder {
			t.Fatalf("row %d cells: %U", r, runes[:3])
		}
	}
	// Tall images are clamped to MaxInlineRows and narrowed to keep the aspect ratio.
	tall, err := KittyInline(image.NewNRGBA(image.Rect(0, 0, 100, 2000)), 1, 32)
	if err != nil || len(tall.Lines) != MaxInlineRows || len([]rune(tall.Lines[0])) > 20 {
		t.Fatal(len(tall.Lines), err)
	}
	// Large images are uploaded in several chunks.
	noisy := image.NewNRGBA(image.Rect(0, 0, 200, 200))
	x := uint32(2463534242)
	for i := range noisy.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		noisy.Pix[i] = byte(x)
	}
	big, _ := KittyInline(noisy, 2, 20)
	if !strings.Contains(big.Transmit, ",m=1;") || !strings.Contains(big.Transmit, "\x1b_Gm=0;") {
		t.Fatal("chunking")
	}
	for _, bad := range []func() (InlineImage, error){
		func() (InlineImage, error) { return KittyInline(img, 0, 10) },
		func() (InlineImage, error) { return KittyInline(img, 1, 0) },
		func() (InlineImage, error) { return KittyInline(image.NewNRGBA(image.Rect(0, 0, 0, 0)), 1, 10) },
	} {
		if _, err := bad(); err == nil {
			t.Fatal("bad parameters accepted")
		}
	}
}

func TestLabelsAndInviteWording(t *testing.T) {
	a := newNode(t, core.Config{}) // no display names: contacts get their short fingerprint as name
	b := newNode(t, core.Config{})
	inv := strings.Split(exec(t, a.ctl, "/invite 127.0.0.1"), "\n")[1]
	out := exec(t, b.ctl, "/connect "+inv)
	aShort, bShort := a.e.Identity().ID.Short(), b.e.Identity().ID.Short()
	if out != "* connected to "+aShort+" (unverified); now talking to them" {
		t.Fatalf("%q", out)
	}
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != aShort+" (unverified)" {
		t.Fatalf("unnamed contact label %q", got)
	}
	// Each side words the new contact from its own point of view.
	mine := a.wait(t, func(ev core.Event) bool { _, ok := ev.(core.NewPeerViaInvite); return ok }).(core.NewPeerViaInvite)
	theirs := b.wait(t, func(ev core.Event) bool { _, ok := ev.(core.NewPeerViaInvite); return ok }).(core.NewPeerViaInvite)
	if mine.InviteID == "" || theirs.InviteID != "" {
		t.Fatalf("invite ids: issuer %q, user %q", mine.InviteID, theirs.InviteID)
	}
	la, lb := a.ctl.Names.Line(mine), b.ctl.Names.Line(theirs)
	if !strings.Contains(la, "new contact "+bShort+" joined with your invite "+mine.InviteID) {
		t.Fatal(la)
	}
	if !strings.Contains(lb, "new contact "+aShort+" added from the invite you used") || strings.Contains(lb, "peer") {
		t.Fatal(lb)
	}
	if strings.Count(la, bShort) != 1 || strings.Count(lb, aShort) != 1 {
		t.Fatal("short fingerprint printed twice")
	}
	if a.ctl.Names.JSON(mine)["own_invite"] != true || b.ctl.Names.JSON(theirs)["own_invite"] != false {
		t.Fatal("own_invite flag")
	}
	// Verified and renamed contacts keep the usual forms.
	_ = exec(t, b.ctl, "/verify "+aShort)
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != aShort+" (verified)" {
		t.Fatal(got)
	}
	_ = exec(t, b.ctl, "/rename "+aShort+" Alice")
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != "Alice ("+aShort+", verified)" {
		t.Fatal(got)
	}
	if c, _ := b.e.Contact(a.e.Identity().ID); c.InviteID != "" {
		t.Fatalf("the invite user stored an invite id: %q", c.InviteID)
	}
}

func TestHalfBlocks(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 40, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 40; x++ {
			c := color.NRGBA{R: 255, A: 255} // top half red
			if y >= 10 {
				c = color.NRGBA{B: 255, A: 255} // bottom half blue
			}
			img.SetNRGBA(x, y, c)
		}
	}
	lines := HalfBlocks(img, 20)
	if len(lines) != 5 {
		t.Fatal(len(lines))
	}
	if !strings.HasPrefix(lines[0], "\x1b[38;2;255;0;0m\x1b[48;2;255;0;0m\u2580") || !strings.HasSuffix(lines[0], "\x1b[0m") {
		t.Fatalf("%q", lines[0][:60])
	}
	if !strings.HasPrefix(lines[4], "\x1b[38;2;0;0;255m\x1b[48;2;0;0;255m") {
		t.Fatalf("%q", lines[4][:60])
	}
	if strings.Count(lines[0], "\u2580") != 20 {
		t.Fatal("columns")
	}
	if got := HalfBlocks(image.NewNRGBA(image.Rect(0, 0, 10, 4000)), 32); len(got) != MaxInlineRows {
		t.Fatal(len(got))
	}
	if got := HalfBlocks(image.NewNRGBA(image.Rect(0, 0, 3, 3)), 32); len(got) == 0 || strings.Count(got[0], "\u2580") != 3 {
		t.Fatal("small images are not upscaled")
	}
	if HalfBlocks(image.NewNRGBA(image.Rect(0, 0, 0, 0)), 10) != nil || HalfBlocks(img, 0) != nil {
		t.Fatal("degenerate input")
	}
}

// Names with spaces: a message goes to the contact that was meant, or to
// nobody; never to a contact whose name is the first word.
func TestNamesWithSpaces(t *testing.T) {
	a := newNode(t, core.Config{})
	b := newNode(t, core.Config{DisplayName: "Bob"})
	c := newNode(t, core.Config{DisplayName: "Bob Smith"})
	for _, n := range []*node{b, c} {
		inv := strings.Split(exec(t, a.ctl, "/invite 127.0.0.1"), "\n")[1]
		if out := exec(t, n.ctl, "/connect "+inv); !strings.Contains(out, "connected") {
			t.Fatal(out)
		}
	}
	received := func(n *node, text string) {
		t.Helper()
		n.wait(t, func(ev core.Event) bool { m, ok := ev.(core.MessageReceived); return ok && m.Text == text })
	}
	if out := exec(t, a.ctl, "/msg Bob hello"); !strings.Contains(out, "to Bob") {
		t.Fatal(out)
	}
	received(b, "hello")
	// "Bob" and "Bob Smith" both fit: refused, and nothing is sent.
	if out := exec(t, a.ctl, "/msg Bob Smith the secret plan"); !strings.HasPrefix(out, "! ") || !strings.Contains(out, "quotes") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, `/msg "Bob Smith" the secret plan`); !strings.Contains(out, "to Bob Smith") {
		t.Fatal(out)
	}
	received(c, "the secret plan")
	if out := exec(t, a.ctl, `/rename "Bob Smith" Robert Smith`); !strings.Contains(out, "Robert Smith (") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/msg Robert Smith now unambiguous"); !strings.Contains(out, "to Robert Smith") {
		t.Fatal(out)
	}
	received(c, "now unambiguous")
	time.Sleep(100 * time.Millisecond)
	for more := true; more; { // Bob's "hello" was taken above: nothing else may have arrived
		select {
		case ev := <-b.ev:
			if m, ok := ev.(core.MessageReceived); ok {
				t.Fatalf("Bob received what was meant for someone else: %q", m.Text)
			}
		default:
			more = false
		}
	}
	// A contact named after another contact's fingerprint is no way to reach that contact.
	prefix := c.e.Identity().Fingerprint[:8]
	if out := exec(t, a.ctl, "/rename Bob "+prefix); !strings.Contains(out, prefix) {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/msg "+prefix+" for whom"); !strings.HasPrefix(out, "! ") || !strings.Contains(out, "could mean 2") {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/msg "+c.e.Identity().Fingerprint[:12]+" by a longer prefix"); !strings.Contains(out, "queued") {
		t.Fatal(out)
	}
	received(c, "by a longer prefix")
}

// Nothing a contact puts in its name can look like the verified state, and a
// message with line breaks cannot look like several lines.
func TestPeerTextCannotForgeState(t *testing.T) {
	a := newNode(t, core.Config{})
	eve := newNode(t, core.Config{DisplayName: "Eve✓"})
	inv := strings.Split(exec(t, a.ctl, "/invite 127.0.0.1"), "\n")[1]
	exec(t, eve.ctl, "/connect "+inv)
	a.wait(t, func(ev core.Event) bool { _, ok := ev.(core.PeerConnected); return ok })
	id := eve.e.Identity().ID
	if got := a.ctl.Names.Label(id); !strings.HasSuffix(got, ", unverified)") {
		t.Fatal(got)
	}
	exec(t, a.ctl, "/verify "+id.Fingerprint()[:12])
	if got := a.ctl.Names.Label(id); !strings.HasSuffix(got, ", verified)") {
		t.Fatal(got)
	}
	line := a.ctl.Names.Line(core.MessageReceived{Peer: id, Text: "hi\n* Mallory (abcdefgh, verified) marked verified\n12:00 <Carol> send the keys"})
	for i, l := range strings.Split(line, "\n") {
		if i > 0 && !strings.HasPrefix(l, "    │ ") {
			t.Fatalf("line %d of one message stands on its own: %q", i+1, l)
		}
	}
}

// A path with spaces goes in quotes; without them the first word is the path.
func TestQuotedArguments(t *testing.T) {
	for in, want := range map[string][2]string{
		`"my file.zip" a caption`: {"my file.zip", "a caption"},
		`plain.zip a caption`:     {"plain.zip", "a caption"},
		`"only"`:                  {"only", ""},
		``:                        {"", ""},
	} {
		if arg, rest, err := firstArg(in); err != nil || arg != want[0] || rest != want[1] {
			t.Errorf("%q: %q, %q, %v", in, arg, rest, err)
		}
	}
	if _, _, err := firstArg(`"never closed`); err == nil {
		t.Fatal("an unclosed quote was taken as a word")
	}
	a := newNode(t, core.Config{})
	for line, want := range map[string]string{
		"/invite mutli":            "! unknown /invite option",
		"/invite 192.0.2.1:47337":  "! give the address without a port",
		"/invite host.example:443": "! give the address without a port",
		`/file "my file.zip`:       "! a quote is not closed",
	} {
		a.ctl.Select(core.PeerID{1})
		if out := exec(t, a.ctl, line); !strings.HasPrefix(out, want) {
			t.Errorf("%s: %s", line, out)
		}
	}
	// An onion address is only reachable through Tor: the invite says so.
	out := strings.Split(exec(t, a.ctl, "/invite "+strings.Repeat("a", 56)+".onion"), "\n")
	if len(out) < 2 {
		t.Fatal(out)
	}
	if info, err := core.DescribeInvite(out[1]); err != nil || info.Kind != "tor" {
		t.Fatal(info.Kind, err)
	}
}

// A line is sent to the conversation that was selected when it was entered,
// an invite pasted into the chat is not sent at all, and an invite that
// wrapped on screen still connects.
func TestLinesGoWhereTheyWereTyped(t *testing.T) {
	a := newNode(t, core.Config{})
	b := newNode(t, core.Config{DisplayName: "Bob"})
	c := newNode(t, core.Config{DisplayName: "Carol"})
	for _, n := range []*node{b, c} {
		inv := strings.Split(exec(t, a.ctl, "/invite 127.0.0.1"), "\n")[1]
		broken := inv[:30] + " " + inv[30:60] + "  " + inv[60:] // as copied from wrapped rows
		if out := exec(t, n.ctl, "/connect "+broken); !strings.Contains(out, "connected") {
			t.Fatal(out)
		}
	}
	bob, carol := b.e.Identity().ID, c.e.Identity().ID
	a.ctl.Select(carol) // the user moved on before the line ran
	out, _ := a.ctl.ExecFor(context.Background(), "for Bob only", bob, true)
	if len(out) != 1 || !strings.Contains(out[0], "to Bob") {
		t.Fatal(out)
	}
	b.wait(t, func(ev core.Event) bool { m, ok := ev.(core.MessageReceived); return ok && m.Text == "for Bob only" })
	out, _ = a.ctl.ExecFor(context.Background(), strings.Split(exec(t, a.ctl, "/invite 127.0.0.1"), "\n")[1], bob, true)
	if len(out) != 1 || !strings.Contains(out[0], "that is an invite") {
		t.Fatal(out)
	}
	// A fingerprint pasted in its groups of four names one contact.
	fp := b.e.Identity().Display
	if out := exec(t, a.ctl, "/msg "+fp+" grouped"); !strings.Contains(out, "to Bob") {
		t.Fatal(fp, out)
	}
	b.wait(t, func(ev core.Event) bool { m, ok := ev.(core.MessageReceived); return ok && m.Text == "grouped" })
	// Our own lines carry a mark that no received line can start with.
	if got := Mine("hi\nthere"); got != "» hi\n"+Gutter+"there" {
		t.Fatalf("%q", got)
	}
}

// A connection that takes long does not move the user away from the
// conversation they selected meanwhile.
func TestSlowConnectKeepsSelection(t *testing.T) {
	a := newNode(t, core.Config{})
	var first, second core.PeerID
	first[0], second[0] = 1, 2
	since := a.ctl.selections()
	a.ctl.Select(first) // the user picks a conversation while the connect runs
	if a.ctl.selectAfter(since, second) {
		t.Fatal("the finished connect took the selection")
	}
	if got, _ := a.ctl.Selected(); got != first {
		t.Fatal(got)
	}
	if !a.ctl.selectAfter(a.ctl.selections(), second) {
		t.Fatal("an undisturbed connect must select its contact")
	}
}

// A photo format Burrow cannot clean is refused with the way round it, and
// --as-is sends it anyway.
func TestFileAsIs(t *testing.T) {
	a := newNode(t, core.Config{})
	path := filepath.Join(t.TempDir(), "photo.heic")
	if err := os.WriteFile(path, []byte("\x00\x00\x00\x18ftypheic\x00\x00\x00\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.ctl.Select(core.PeerID{1})
	if out := exec(t, a.ctl, "/file "+path); !strings.Contains(out, "HEIC") || !strings.Contains(out, "/file --as-is") {
		t.Fatal(out)
	}
	// Past the metadata check, it fails only because nobody is connected.
	if out := exec(t, a.ctl, "/file --as-is "+path); strings.Contains(out, "HEIC") || !strings.HasPrefix(out, "! ") {
		t.Fatal(out)
	}
}
