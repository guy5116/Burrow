package tui

import (
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

var fast = store.Options{Argon2: store.Argon2{Time: 1, MemKiB: store.MinMemKiB, Threads: 1}}

func newModel(t *testing.T) *model {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Init(dir, []byte("pw"), fast)
	if err != nil {
		t.Fatal(err)
	}
	e, err := core.New(core.Config{ListenAddr: "127.0.0.1:0", DataDir: dir}, st, core.Transports("127.0.0.1:0", nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Start(ctx); close(done) }()
	for len(e.ListenAddrs()) == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(func() { cancel(); <-done; st.Close() })
	in := textinput.New()
	in.Focus()
	return &model{ctx: ctx, ctl: common.NewController(e, "127.0.0.1"), addrs: "127.0.0.1:1", logs: map[core.PeerID][]string{}, vp: viewport.New(), in: in}
}

func key(code rune, text string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Text: text} }

func runCmd(t *testing.T, m *model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if msg != nil {
			if _, isBatch := msg.(tea.BatchMsg); !isBatch {
				m.Update(msg)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not finish")
	}
}

func TestModelLifecycle(t *testing.T) {
	m := newModel(t)
	if cmd := m.Init(); cmd == nil {
		t.Fatal("Init returned no command")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.w != 100 || m.vp.Height() != 27 {
		t.Fatal(m.w, m.vp.Height())
	}
	m.Update(tea.WindowSizeMsg{Width: 10, Height: 2}) // tiny terminals clamp
	if m.vp.Width() != 20 || m.vp.Height() != 3 {
		t.Fatal(m.vp.Width(), m.vp.Height())
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	if !v.AltScreen || !strings.Contains(v.Content, "contacts") || !strings.Contains(v.Content, "me ") {
		t.Fatalf("%q", v.Content)
	}

	// Typing goes to the input; enter runs the command off the render thread.
	for _, r := range "/id" {
		m.Update(key(r, string(r)))
	}
	if m.in.Value() != "/id" {
		t.Fatal(m.in.Value())
	}
	_, cmd := m.Update(key(tea.KeyEnter, ""))
	if !m.busy || m.in.Value() != "" {
		t.Fatal("enter did not submit")
	}
	runCmd(t, m, cmd)
	if m.busy || !strings.Contains(strings.Join(m.system, "\n"), "your fingerprint") {
		t.Fatal(m.system)
	}
	// Blank enter does nothing; /quit and ctrl+c quit.
	if _, cmd := m.Update(key(tea.KeyEnter, "")); cmd != nil {
		t.Fatal("blank enter produced a command")
	}
	m.in.SetValue("/quit")
	if _, cmd := m.Update(key(tea.KeyEnter, "")); cmd == nil {
		t.Fatal("no quit command")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c did not quit")
	}
	if _, cmd := m.Update(outMsg{quit: true}); cmd == nil {
		t.Fatal("quit outMsg")
	}
	m.Update(key(tea.KeyPgUp, ""))
	m.Update(key(tea.KeyPgDown, ""))
	m.Update(key(tea.KeyTab, "")) // no contacts: no-op
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.ctl.Current != nil {
		t.Fatal("selection without contacts")
	}
}

func TestModelEvents(t *testing.T) {
	m := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	var p core.PeerID
	p[0] = 4
	m.ctl.Current = &p
	var id core.TransferID
	id[0] = 2
	tp := core.TransferPeer{Peer: p}
	for _, ev := range []core.Event{
		core.PeerConnected{Peer: p, Transport: "tcp"},
		core.MessageReceived{Peer: p, ID: 1, Text: "hello there"},
		core.MessageStatus{Peer: p, ID: 2, Status: core.StatusPending},
		core.MessageStatus{Peer: p, ID: 2, Status: core.StatusDelivered},
		core.Typing{Peer: p, Typing: true}, core.Typing{Peer: p},
		core.ImageOffered{Peer: p, ID: id, Size: 1 << 20, Format: 1, Width: 3, Height: 4, Caption: "cap"},
		core.TransferDone{Peer: tp, ID: id, Path: "/tmp/x.png"},
		core.TransferDone{Peer: core.TransferPeer{Peer: p, Outgoing: true}},
		core.TransferFailed{Peer: tp, Reason: "declined"},
		core.NameCollision{New: p, Existing: p, Name: "n"}, core.HandshakeFailed{Stage: "msg2", Reason: "r"},
		core.ErrorEvent{Message: "oops"},
	} {
		m.Update(evMsg{ev})
	}
	log := strings.Join(m.logs[p], "\n")
	for _, want := range []string{"hello there", "image offer #1", "/accept 1", "image saved: /tmp/x.png", "/view 1", "image delivered", "declined", "typing"} {
		if !strings.Contains(log, want) {
			t.Errorf("conversation lacks %q:\n%s", want, log)
		}
	}
	sys := strings.Join(m.system, "\n")
	for _, want := range []string{"connected", "NAME COLLISION", "could not establish", "oops"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system log lacks %q", want)
		}
	}
	if v := m.View().Content; !strings.Contains(v, "hello there") {
		t.Fatal("conversation not rendered")
	}
	// /view: bad number, then a terminal without inline images.
	for _, k := range []string{"KITTY_WINDOW_ID", "TERM", "TERM_PROGRAM"} {
		t.Setenv(k, "")
	}
	runCmd(t, m, m.view("9"))
	runCmd(t, m, m.view("1"))
	sys = strings.Join(m.system, "\n")
	if !strings.Contains(sys, "usage: /view") || !strings.Contains(sys, "cannot show images inline") {
		t.Fatal(sys)
	}
	t.Setenv("TERM", "xterm-kitty")
	runCmd(t, m, m.view("1")) // file does not exist → decode error line
	if !strings.Contains(strings.Join(m.system, "\n"), "cannot decode image") {
		t.Fatal(m.system)
	}
	m.Update(viewMsg{}) // nothing pending: no-op
	if captionOf("") != "" || captionOf("x") != ` "x"` || padRight("ab", 4) != "ab  " || padRight("abcd", 2) != "abcd" {
		t.Fatal("helpers")
	}
	m.busy = true
	if !strings.Contains(m.View().Content, "working") {
		t.Fatal("busy marker")
	}
}

func TestSelectionCyclesContacts(t *testing.T) {
	m := newModel(t)
	var a, b core.PeerID
	a[0], b[0] = 1, 2
	m.contacts = []core.Contact{{ID: a, Nickname: "a", Online: true, Verified: true}, {ID: b, Nickname: strings.Repeat("long", 12)}}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.selectOffset(1)
	if m.ctl.Current == nil || *m.ctl.Current != a {
		t.Fatal("first selection")
	}
	m.selectOffset(1)
	if *m.ctl.Current != b {
		t.Fatal("next")
	}
	m.selectOffset(1)
	if *m.ctl.Current != a {
		t.Fatal("wrap")
	}
	m.selectOffset(-1)
	if *m.ctl.Current != b {
		t.Fatal("previous wraps")
	}
	// The sidebar shows markers and truncates long names (contacts come from the engine on refresh,
	// so render directly from the injected list).
	side := m.View().Content
	if !strings.Contains(side, "contacts") {
		t.Fatal(side)
	}
}

func TestRunQuitsOnContextCancel(t *testing.T) {
	m := newModel(t)
	ctx, cancel := context.WithCancel(m.ctx)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, m.ctl, core.Target{Name: "nobody"}, m.ctl.E.ListenAddrs(),
			tea.WithInput(strings.NewReader("")), tea.WithOutput(io.Discard), tea.WithoutSignals(), tea.WithWindowSize(80, 24))
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal(code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not quit on cancel")
	}
}

func TestInlineThumbnailFlow(t *testing.T) {
	m := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	var p core.PeerID
	p[0] = 8
	m.ctl.Current = &p
	// A PNG on disk, as TransferDone would report it.
	path := filepath.Join(t.TempDir(), "img-abcd.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 64, 32))); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for _, k := range []string{"KITTY_WINDOW_ID", "TERM_PROGRAM"} {
		t.Setenv(k, "")
	}
	// Without Kitty: only the saved path appears.
	t.Setenv("TERM", "xterm-256color")
	if _, cmd := m.Update(evMsg{core.TransferDone{Peer: core.TransferPeer{Peer: p}, Path: path}}); cmd != nil {
		t.Fatal("inline command issued for a terminal without image support")
	}
	// With Kitty: the thumbnail is prepared off the render thread, its rows join the conversation
	// and the upload is sent raw.
	t.Setenv("TERM", "xterm-kitty")
	_, cmd := m.Update(evMsg{core.TransferDone{Peer: core.TransferPeer{Peer: p}, Path: path}})
	if cmd == nil {
		t.Fatal("no inline command on Kitty")
	}
	msg := cmd()
	in, ok := msg.(inlineMsg)
	if !ok || in.peer != p || len(in.img.Lines) == 0 {
		t.Fatalf("%T", msg)
	}
	before := len(m.logs[p])
	_, raw := m.Update(in)
	if raw == nil || len(m.logs[p]) != before+len(in.img.Lines) {
		t.Fatal("placeholder rows not added or upload not issued")
	}
	if !strings.ContainsRune(m.logs[p][len(m.logs[p])-1], 0x10EEEE) {
		t.Fatal("placeholder cells missing")
	}
	// A missing file fails silently (the path line is already there).
	if got := m.inline(p, filepath.Join(t.TempDir(), "gone.png"), common.TermKitty)(); got != nil {
		t.Fatalf("%v", got)
	}
	// Ids cycle through 1–255 and never use 0.
	m.nextImage = 255
	_ = m.inline(p, path, common.TermKitty)
	if m.nextImage != 1 {
		t.Fatal(m.nextImage)
	}
	if !strings.Contains(m.View().Content, "me ") {
		t.Fatal("status bar")
	}
}

func TestHalfBlockPreviewOnITerm(t *testing.T) {
	m := newModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	var p core.PeerID
	p[0] = 3
	m.ctl.Current = &p
	path := filepath.Join(t.TempDir(), "img-ffff.png")
	f, _ := os.Create(path)
	_ = png.Encode(f, image.NewNRGBA(image.Rect(0, 0, 64, 64)))
	_ = f.Close()
	for _, k := range []string{"KITTY_WINDOW_ID", "TERM"} {
		t.Setenv(k, "")
	}
	t.Setenv("TERM_PROGRAM", "iTerm.app")
	_, cmd := m.Update(evMsg{core.TransferDone{Peer: core.TransferPeer{Peer: p}, Path: path}})
	if cmd == nil {
		t.Fatal("no inline preview on iTerm2")
	}
	in, ok := cmd().(inlineMsg)
	if !ok || in.img.Transmit != "" || len(in.img.Lines) == 0 {
		t.Fatalf("%+v", in)
	}
	if _, raw := m.Update(in); raw != nil {
		t.Fatal("half-block preview must not send raw escapes")
	}
	if !strings.Contains(m.logs[p][len(m.logs[p])-1], "\u2580") {
		t.Fatal("preview rows missing")
	}
}
