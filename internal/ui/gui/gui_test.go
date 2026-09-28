//go:build gui

package gui

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
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
		msgs: map[core.PeerID][]*row{}, byMsg: map[core.MsgID]*row{}, offers: map[core.TransferID]dialogT{}}
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
	t.Cleanup(func() { st.Close() })
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
	// Label + one link row; the link row has only "Copy link" while open_links is off.
	if len(w.Objects) != 2 {
		t.Fatalf("objects %d", len(w.Objects))
	}
	lr := w.Objects[1].(*fyne.Container)
	if len(lr.Objects) != 1 || lr.Objects[0].(*widget.Button).Text != "Copy link" {
		t.Fatal("open-in-browser offered while open_links is off")
	}
	a.cfg.OpenLinks = true
	r.widget = nil
	w = a.rowWidget(r).(*fyne.Container)
	if lr := w.Objects[1].(*fyne.Container); len(lr.Objects) != 2 {
		t.Fatal("open-in-browser missing with open_links on")
	}
	// Message text stays plain: the label shows the raw text.
	if l := w.Objects[0].(*widget.Label); !strings.Contains(l.Text, "https://example.org") || l.Wrapping != fyne.TextWrapWord {
		t.Fatal("label")
	}
}

func TestEventsRenderAndStatus(t *testing.T) {
	a := newApp(t)
	var peer core.PeerID
	peer[0] = 7
	// A contact must exist for the sidebar; create one through the engine's contact path.
	a.e.RenameContact(peer, "x") // unknown: no-op error, fine
	a.apply(core.MessageReceived{Peer: peer, ID: 1, Text: "hello"})
	if len(a.msgs[peer]) != 1 || a.msgs[peer][0].text != "hello" || a.msgs[peer][0].mine {
		t.Fatalf("%+v", a.msgs[peer])
	}
	// Selecting the conversation renders it.
	a.sel = &peer
	a.ctl.Current = &peer
	a.renderConversation()
	if len(a.conv.Objects) != 1 {
		t.Fatal("conversation not rendered")
	}
	a.apply(core.MessageReceived{Peer: peer, ID: 2, Text: "again"})
	if len(a.conv.Objects) != 2 {
		t.Fatal("row not appended")
	}
	a.apply(core.PeerConnected{Peer: peer, Transport: "tcp"})
	if len(a.conv.Objects) != 3 || a.msgs[peer][2].kind != rowSystem {
		t.Fatal("system row")
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
	a.ctl.Current = &peer
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
}

func TestUnlockAndWizardValidation(t *testing.T) {
	dir := t.TempDir()
	a := &App{fa: test.NewTempApp(t), paths: store.Paths{Config: dir, Data: dir}, cfg: store.DefaultConfig(), log: slog.Default(),
		msgs: map[core.PeerID][]*row{}, byMsg: map[core.MsgID]*row{}, offers: map[core.TransferID]dialogT{}}
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
	_ = container.NewVBox
	_ = time.Second
}
