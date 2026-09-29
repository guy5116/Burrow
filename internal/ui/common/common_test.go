package common

import (
	"bytes"
	"context"
	"image"
	"net"
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
	if out := exec(t, b.ctl, "/connect "+lines[1]); !strings.Contains(out, "connected to") || b.ctl.Current == nil {
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
	if out := exec(t, a.ctl, "/contacts"); !strings.Contains(out, "verified ✓") {
		t.Fatal(out)
	}
	if label := a.ctl.Names.Label(b.e.Identity().ID); !strings.Contains(label, "Bob✓") {
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
	if out := exec(t, a.ctl, "/typing on"); !strings.Contains(out, "on") || !a.ctl.Typing {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/typing off"); !strings.Contains(out, "off") || a.ctl.Typing {
		t.Fatal(out)
	}
	if out := exec(t, a.ctl, "/history 5"); !strings.Contains(out, "history is off") {
		t.Fatal(out)
	}

	// Transfers by number.
	if out := exec(t, a.ctl, "/transfers"); out != "(no pending offers)" {
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
	a.ctl.Observe(core.ImageOffered{ID: id})
	if a.ctl.Number(id) != 1 || !strings.Contains(exec(t, a.ctl, "/transfers"), "#1 07") {
		t.Fatal("offer numbering")
	}
	a.ctl.Observe(core.TransferDone{ID: id, Path: "/x/img.png"})
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
	if out := exec(t, a.ctl, "/remove Robert"); !strings.Contains(out, "removed") || a.ctl.Current != nil {
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
	b.ctl.Current = nil
	if st := b.ctl.Status(); st != "me "+b.e.Identity().ID.Short() {
		t.Fatal(st)
	}
	var gone core.PeerID
	b.ctl.Current = &gone
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
	if out != "* connected to "+aShort+"; now talking to them" {
		t.Fatalf("%q", out)
	}
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != aShort {
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
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != aShort+"✓" {
		t.Fatal(got)
	}
	_ = exec(t, b.ctl, "/rename "+aShort+" Alice")
	if got := b.ctl.Names.Label(a.e.Identity().ID); got != "Alice✓ ("+aShort+")" {
		t.Fatal(got)
	}
	if c, _ := b.e.Contact(a.e.Identity().ID); c.InviteID != "" {
		t.Fatalf("the invite user stored an invite id: %q", c.InviteID)
	}
}
