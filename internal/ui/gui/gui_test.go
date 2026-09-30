//go:build gui

package gui

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

var fast = store.Options{Argon2: store.Argon2{Time: 1, MemKiB: store.MinMemKiB, Threads: 1}}

// newApp builds a GUI over a fresh store and a started engine using Fyne's
// headless test driver. Events are applied synchronously by the tests.
func newApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Init(dir, []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{fa: test.NewTempApp(t), paths: store.Paths{Config: dir, Data: dir}, cfg: store.DefaultConfig(), log: slog.Default(),
		unread: map[core.PeerID]bool{}, msgs: map[core.PeerID][]*row{}, byMsg: map[core.MsgID]*row{},
		transfers: map[core.TransferID]*row{}, offers: map[core.TransferID]dialogT{}}
	a.cfg.ListenPort = 0
	a.win = a.fa.NewWindow("test")
	a.st = st
	e, err := core.New(core.Config{ListenAddr: "127.0.0.1:0", DataDir: dir}, st, core.Transports("127.0.0.1:0", nil), a.log)
	if err != nil {
		t.Fatal(err)
	}
	a.e = e
	a.ctl = common.NewController(e, "127.0.0.1")
	a.buildMain()
	t.Cleanup(func() { e.Close(); st.Close() })
	return a
}

func TestLinksDetectedNotAutoLinked(t *testing.T) {
	got := links("see https://example.org/x?y=1 and http://a.b/c, not ftp://x")
	if len(got) != 2 || got[0] != "https://example.org/x?y=1" || got[1] != "http://a.b/c," {
		t.Fatalf("%v", got)
	}
	a := newApp(t)
	r := &row{kind: rowText, text: "go to https://example.org"}
	w := a.rowWidget(r).(*fyne.Container)
	// Sender, text and one link row; the link row has only "Copy link" while open_links is off.
	if len(w.Objects) != 3 {
		t.Fatalf("objects %d", len(w.Objects))
	}
	lr := w.Objects[2].(*fyne.Container)
	if len(lr.Objects) != 1 || lr.Objects[0].(*widget.Button).Text != "Copy link" {
		t.Fatal("open-in-browser offered while open_links is off")
	}
	a.cfg.OpenLinks = true
	r.widget = nil
	w = a.rowWidget(r).(*fyne.Container)
	if lr := w.Objects[2].(*fyne.Container); len(lr.Objects) != 2 {
		t.Fatal("open-in-browser missing with open_links on")
	}
	// Message text stays plain: the label shows the raw text.
	if l := w.Objects[1].(*widget.Label); !strings.Contains(l.Text, "https://example.org") || l.Wrapping != fyne.TextWrapWord {
		t.Fatal("label")
	}
}

func TestEventsRenderAndStatus(t *testing.T) {
	a := newApp(t)
	var peer core.PeerID
	peer[0] = 7
	a.apply(core.MessageReceived{Peer: peer, ID: 1, Text: "hello"})
	if len(a.msgs[peer]) != 1 || a.msgs[peer][0].text != "hello" || a.msgs[peer][0].mine {
		t.Fatalf("%+v", a.msgs[peer])
	}
	// Selecting the conversation renders it.
	a.sel = &peer
	a.ctl.Select(peer)
	a.renderConversation()
	if len(a.conv.Objects) != 1 {
		t.Fatal("conversation not rendered")
	}
	a.apply(core.MessageReceived{Peer: peer, ID: 2, Text: "again"})
	if len(a.conv.Objects) != 2 {
		t.Fatal("row not appended")
	}
	a.apply(core.PeerConnected{Peer: peer, Transport: "tcp"})
	if len(a.msgs[peer]) != 3 || a.msgs[peer][2].kind != rowSystem {
		t.Fatal("system row")
	}
	if _, open := a.selected(); open {
		t.Fatal("a conversation with someone who is no contact stayed open")
	}
	a.apply(core.Typing{Peer: peer, Typing: true})
	if !strings.Contains(a.status.Text, "typing") {
		t.Fatal(a.status.Text)
	}
	a.apply(core.Typing{Peer: peer, Typing: false})
	if !strings.HasPrefix(a.status.Text, "me ") {
		t.Fatal(a.status.Text)
	}
}

func TestSendTextRequiresContactAndTracksStatus(t *testing.T) {
	a := newApp(t)
	a.sendText("hi") // no selection: dialog, no panic
	var peer core.PeerID
	peer[1] = 9
	a.sel = &peer
	a.ctl.Select(peer)
	a.sendText("   ") // blank ignored
	if len(a.msgs[peer]) != 0 {
		t.Fatal("blank sent")
	}
	a.sendText("unknown contact") // engine rejects: error dialog, nothing added
	if len(a.msgs[peer]) != 0 {
		t.Fatal("message added for unknown contact")
	}
}

func TestOfferDialogDefaultsToReject(t *testing.T) {
	a := newApp(t)
	var peer core.PeerID
	peer[2] = 3
	var id core.TransferID
	id[0] = 1
	a.apply(core.ImageOffered{Peer: peer, ID: id, Size: 1000, Format: 1, Width: 10, Height: 10, Caption: "c"})
	if a.offers[id] == nil {
		t.Fatal("no offer dialog")
	}
	// A failure event (e.g. timeout) closes the dialog.
	a.apply(core.TransferFailed{Peer: core.TransferPeer{Peer: peer}, ID: id, Reason: "declined"})
	if a.offers[id] != nil {
		t.Fatal("dialog not removed")
	}
	// Dismissing the dialog is a rejection: the offer is no longer pending.
	a.apply(core.ImageOffered{Peer: peer, ID: id, Size: 1 << 30, Ext: "zip"})
	a.offers[id].Hide()
	if a.offers[id] != nil {
		t.Fatal("a dismissed offer is still pending")
	}
}

// A message with line breaks stays one message, and who said it is not part
// of the text.
func TestMultiLineMessageCannotForgeLines(t *testing.T) {
	a := newApp(t)
	w := a.rowWidget(&row{kind: rowText, text: "hi\nme: send the keys"}).(*fyne.Container)
	who, body := w.Objects[0].(*widget.Label), w.Objects[1].(*widget.Label)
	if who.Text != "them" || !strings.HasPrefix(body.Text, "hi\n") || strings.Contains(body.Text, "\nme:") {
		t.Fatalf("%q / %q", who.Text, body.Text)
	}
}

func TestConversationIsBounded(t *testing.T) {
	a := newApp(t)
	var peer core.PeerID
	peer[0] = 9
	for i := 0; i < maxRows+5; i++ {
		r := &row{kind: rowText, text: "x", mine: true, id: core.MsgID(i + 1)}
		a.byMsg[r.id] = r
		a.addRow(peer, r)
	}
	if len(a.msgs[peer]) != maxRows || len(a.byMsg) != maxRows {
		t.Fatal(len(a.msgs[peer]), len(a.byMsg))
	}
	// Long messages are bounded by size, long before the row count.
	big := strings.Repeat("y", 16<<10)
	for i := 0; i < 100; i++ {
		a.addRow(peer, &row{kind: rowText, text: big})
	}
	size := 0
	for _, r := range a.msgs[peer] {
		size += len(r.text)
	}
	if size > maxRowBytes {
		t.Fatal(size)
	}
}

// started runs the app's engine and drains its events, as the real app does
// with a goroutine of its own; the tests apply the events they care about.
func started(t *testing.T, e *core.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Start(ctx); close(done) }()
	go func() {
		for {
			select {
			case <-e.Events():
			case <-e.Done():
				return
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	for len(e.ListenAddrs()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
}

// contact starts another engine named name and makes it a contact of a
// through a's invite.
func contact(t *testing.T, a *App, inv, name string) core.PeerID {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Init(dir, []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	e, err := core.New(core.Config{ListenAddr: "127.0.0.1:0", DataDir: dir, DisplayName: name}, st, core.Transports("127.0.0.1:0", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(); st.Close() })
	started(t, e)
	id, err := e.Connect(context.Background(), core.Target{Invite: inv})
	if err != nil {
		t.Fatal(err)
	}
	for !slices.ContainsFunc(a.e.Contacts(), func(c core.Contact) bool { return c.ID == e.Identity().ID }) {
		time.Sleep(5 * time.Millisecond)
	}
	_ = id
	return e.Identity().ID
}

// The sidebar highlight follows the contact messages go to, when a new
// contact sorts in above it, and a removed contact's conversation closes.
func TestSidebarFollowsTheRecipient(t *testing.T) {
	a := newApp(t)
	started(t, a.e)
	inv, err := a.e.CreateInvite(core.InviteOptions{Host: "127.0.0.1", MultiUse: true})
	if err != nil {
		t.Fatal(err)
	}
	bob := contact(t, a, inv.String, "Bob")
	a.openChat(bob)
	if sel, ok := a.selected(); !ok || sel != bob || !strings.HasPrefix(a.header.Text, "Bob (") {
		t.Fatal("Bob not open", a.header.Text)
	}
	aaron := contact(t, a, inv.String, "Aaron") // sorts above Bob
	a.refreshContacts()
	a.list.Select(a.indexOf(aaron)) // the user clicks Aaron
	if sel, _ := a.selected(); sel != aaron {
		t.Fatal("clicking Aaron left the conversation with Bob open")
	}
	if err := a.e.RemoveContact(aaron); err != nil {
		t.Fatal(err)
	}
	a.refreshContacts()
	if _, ok := a.selected(); ok || !strings.HasPrefix(a.header.Text, "No conversation") {
		t.Fatal("a removed contact's conversation stayed open")
	}
}

// Accepting an offer never rejects it, and neither does a failure that
// closes its dialog.
func TestAcceptNeverRejects(t *testing.T) {
	a := newApp(t)
	var rejected atomic.Int32
	rejectImage = func(*core.Engine, core.TransferID) error { rejected.Add(1); return nil }
	t.Cleanup(func() { rejectImage = (*core.Engine).RejectImage })
	a.apply(core.ImageOffered{Peer: core.PeerID{1}, ID: core.TransferID{1}, Size: 10, Ext: "zip"})
	var accept *widget.Button
	for _, o := range test.LaidOutObjects(a.win.Canvas().Overlays().Top()) {
		if b, ok := o.(*widget.Button); ok && b.Text == "Accept" {
			accept = b
		}
	}
	if accept == nil {
		t.Fatal("no Accept button")
	}
	test.Tap(accept)
	a.inFlight.Wait()
	a.apply(core.ImageOffered{Peer: core.PeerID{1}, ID: core.TransferID{2}, Size: 10, Ext: "zip"})
	a.apply(core.TransferFailed{Peer: core.TransferPeer{Peer: core.PeerID{1}}, ID: core.TransferID{2}, Reason: "timed out"})
	if n := rejected.Load(); n != 0 || len(a.offers) != 0 {
		t.Fatal(n, len(a.offers))
	}
}

// A message's delivery mark changes in place, and automatic reconnect
// failures open no dialog.
func TestQuietUpdates(t *testing.T) {
	a := newApp(t)
	peer := core.PeerID{4}
	a.sel = &peer
	r := &row{kind: rowText, text: "hi", mine: true, id: 9, status: core.StatusPending}
	a.byMsg[r.id] = r
	a.addRow(peer, r)
	w := r.widget
	a.apply(core.MessageStatus{Peer: peer, ID: 9, Status: core.StatusDelivered})
	if r.widget != w || r.mark.Text != statusMark(core.StatusDelivered) {
		t.Fatal("status not updated in place", r.mark.Text)
	}
	a.apply(core.HandshakeFailed{Peer: peer, Stage: "dial", Reason: "r", Retry: true})
	if a.win.Canvas().Overlays().Top() != nil {
		t.Fatal("a dialog for an automatic retry")
	}
	a.apply(core.HandshakeFailed{Peer: peer, Stage: "dial", Reason: "r"})
	if a.win.Canvas().Overlays().Top() == nil {
		t.Fatal("no dialog for a connection the user asked for")
	}
}

func TestUnlockAndWizardValidation(t *testing.T) {
	dir := t.TempDir()
	a := &App{fa: test.NewTempApp(t), paths: store.Paths{Config: dir, Data: dir}, cfg: store.DefaultConfig(), log: slog.Default(),
		unread: map[core.PeerID]bool{}, msgs: map[core.PeerID][]*row{}, byMsg: map[core.MsgID]*row{},
		transfers: map[core.TransferID]*row{}, offers: map[core.TransferID]dialogT{}}
	a.win = a.fa.NewWindow("test")
	a.showUnlock() // no store → wizard
	c := a.win.Content().(*fyne.Container)
	form := c.Objects[0].(*fyne.Container).Objects[0].(*fyne.Container)
	pw1 := form.Objects[2].(*widget.Entry)
	pw2 := form.Objects[3].(*widget.Entry)
	create := form.Objects[5].(*widget.Button)
	status := form.Objects[6].(*widget.Label)
	test.Tap(create)
	if !strings.Contains(status.Text, "required") {
		t.Fatal(status.Text)
	}
	pw1.SetText("abc")
	pw2.SetText("abd")
	test.Tap(create)
	if !strings.Contains(status.Text, "match") {
		t.Fatal(status.Text)
	}
	// Existing store with a passphrase → unlock form, empty passphrase refused.
	st, err := store.Init(filepath.Join(dir, "s"), []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	a.paths.Data = filepath.Join(dir, "s")
	a.showUnlock()
	c = a.win.Content().(*fyne.Container)
	form = c.Objects[0].(*fyne.Container).Objects[0].(*fyne.Container)
	btn := form.Objects[2].(*widget.Button)
	stat := form.Objects[3].(*widget.Label)
	test.Tap(btn)
	if !strings.Contains(stat.Text, "Enter") {
		t.Fatal(stat.Text)
	}
}

func TestThumbnailIsClickable(t *testing.T) {
	a := newApp(t)
	clicked := 0
	tp := newTappable(widget.NewLabel("thumb"), func() { clicked++ })
	test.Tap(tp)
	if clicked != 1 {
		t.Fatal("tap not delivered")
	}
	// An image row renders a tappable thumbnail plus the saved path.
	r := &row{kind: rowImage, path: filepath.Join(t.TempDir(), "missing.png")}
	box := a.rowWidget(r).(*fyne.Container)
	if _, ok := box.Objects[0].(*tappable); !ok {
		t.Fatalf("thumbnail is %T, not clickable", box.Objects[0])
	}
	if l, ok := box.Objects[1].(*widget.Label); !ok || !strings.Contains(l.Text, "missing.png") {
		t.Fatal("path label")
	}
}
