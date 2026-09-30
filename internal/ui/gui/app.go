//go:build gui

// Package gui is the Fyne desktop UI (CLAUDE.md §10). It imports only
// internal/core (plus ui/common for formatting) and never touches sessions,
// crypto or transports.
package gui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

// App is the running GUI.
type App struct {
	fa    fyne.App
	win   fyne.Window
	paths store.Paths
	cfg   store.Config
	log   *slog.Logger

	st     *store.Store
	e      *core.Engine
	ctl    *common.Controller
	cancel context.CancelFunc
	done   chan struct{}

	// UI model: touched only on the Fyne thread.
	contacts []core.Contact
	sel      *core.PeerID
	msgs     map[core.PeerID][]*row
	byMsg    map[core.MsgID]*row
	list     *widget.List
	conv     *fyne.Container
	scroll   *container.Scroll
	status   *widget.Label
	composer *widget.Entry
	offers   map[core.TransferID]dialog.Dialog
	typingTo *core.PeerID // the contact who was last told that we are typing
}

// maxRows bounds what is kept per conversation; the oldest rows go first.
const maxRows = 5000

// dialog is the interface the offers map holds (kept for tests).
type dialogT = dialog.Dialog

// Run opens the unlock/creation flow and then the main window. It blocks
// until the window closes and returns the process exit code.
func Run(paths store.Paths, cfg store.Config, logger *slog.Logger) int {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	a := &App{fa: app.NewWithID("io.github.guy5116.burrow"), paths: paths, cfg: cfg, log: logger,
		msgs: map[core.PeerID][]*row{}, byMsg: map[core.MsgID]*row{}, offers: map[core.TransferID]dialog.Dialog{}}
	a.win = a.fa.NewWindow("Burrow")
	a.win.Resize(fyne.NewSize(900, 600))
	a.win.SetMaster()
	a.win.SetCloseIntercept(func() { a.shutdown(); a.win.Close() })
	a.showUnlock()
	a.win.ShowAndRun()
	a.shutdown() // Quit, SIGINT and SIGTERM end the run loop without the close intercept
	return 0
}

// async runs work off the Fyne thread and then on it. Everything that may
// wait for the disk, the network or Argon2 goes through here: the window
// stays responsive meanwhile.
func (a *App) async(work func() error, then func(error)) {
	go func() {
		err := work()
		fyne.Do(func() { then(err) })
	}()
}

// confirm asks a question whose safe answer is "no". The button that goes
// ahead is not the emphasised one.
func (a *App) confirm(d *dialog.ConfirmDialog) {
	d.SetConfirmImportance(widget.MediumImportance)
	d.Show()
}

// shutdown stops the engine (BYE to peers) and releases the store.
func (a *App) shutdown() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
		a.cancel = nil
	}
	if a.st != nil {
		a.st.Close()
		a.st = nil
	}
}

// startEngine builds and starts the engine over an unlocked store, then
// switches the window to the main layout.
func (a *App) startEngine(st *store.Store) {
	a.st = st
	ecfg := core.Config{ListenPort: a.cfg.ListenPort, DisplayName: a.cfg.DisplayName, Typing: a.cfg.Typing,
		NoTimestamp: !a.cfg.Timestamps, AutoReconnect: a.cfg.AutoReconnect, DataDir: a.paths.Data,
		ImageDir: a.cfg.ImageDir, MaxImage: uint64(a.cfg.MaxImageMiB) << 20, MaxFile: uint64(a.cfg.MaxFileMiB) << 20, NoFiles: a.cfg.MaxFileMiB == 0, Paranoid: a.cfg.ParanoidImages,
		AutoAcceptFromVerified: a.cfg.AutoAcceptFromVerified, MDNS: a.cfg.MDNS, History: a.cfg.History, MaxPeers: a.cfg.MaxPeers}
	listen := ":" + strconv.Itoa(int(a.cfg.ListenPort))
	var torOpts *core.TorOptions
	if a.cfg.Transport == "tor" || a.cfg.Transport == "both" {
		key, err := st.LoadOnionKey()
		if err != nil {
			dialog.ShowError(err, a.win)
			return
		}
		torOpts = &core.TorOptions{Key: key, Port: a.cfg.ListenPort, Exe: a.cfg.TorExe, DataDir: a.paths.Data}
		if a.cfg.Transport == "tor" {
			listen = ""
		}
	}
	e, err := core.New(ecfg, st, core.Transports(listen, torOpts), a.log)
	if err != nil {
		dialog.ShowError(err, a.win)
		return
	}
	a.e = e
	a.ctl = common.NewController(e, a.cfg.InviteHost)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.done = make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		err := e.Start(ctx)
		e.Close() // a failed Start leaves the engine running
		errc <- err
		close(a.done)
	}()
	// Drain events on a dedicated goroutine; marshal to the UI thread.
	go func() {
		for {
			select {
			case ev := <-e.Events():
				a.ctl.Observe(ev)
				fyne.Do(func() { a.apply(ev) })
			case <-e.Done():
				return
			}
		}
	}()
	go func() {
		if err := <-errc; err != nil {
			fyne.Do(func() { dialog.ShowError(err, a.win) })
		}
	}()
	a.buildMain()
}

// row is one line in a conversation.
type row struct {
	kind   rowKind
	text   string
	mine   bool
	id     core.MsgID // of a message we sent
	status core.Status
	path   string // image file
	widget fyne.CanvasObject
}

type rowKind uint8

const (
	rowText rowKind = iota
	rowSystem
	rowImage
)

var linkRE = regexp.MustCompile(`https?://[^\s<>"']+`)

// links returns the URLs found in a message (shown as text; never auto-linked).
func links(s string) []string { return linkRE.FindAllString(s, 8) }

func (a *App) selected() (core.PeerID, bool) {
	if a.sel == nil {
		return core.PeerID{}, false
	}
	return *a.sel, true
}

// choose makes id the selected conversation in the window and the controller.
func (a *App) choose(id core.PeerID) {
	a.setTyping(false)
	a.sel = &id
	a.ctl.Select(id)
}

// setTyping tells the selected contact that we started or stopped typing,
// once per change.
func (a *App) setTyping(now bool) {
	id, ok := a.selected()
	switch {
	case now && a.typingTo == nil && ok && a.ctl.TypingOn():
		a.e.SetTyping(id, true)
		a.typingTo = &id
	case !now && a.typingTo != nil:
		a.e.SetTyping(*a.typingTo, false)
		a.typingTo = nil
	}
}

func (a *App) label(id core.PeerID) string { return a.ctl.Names.Label(id) }

func (a *App) errDialog(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, core.ErrUnknownContact) {
		err = fmt.Errorf("that contact no longer exists")
	}
	dialog.ShowError(err, a.win)
}
