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
	"sync"

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
	mu       sync.Mutex
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
}

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
	return 0
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
		ImageDir: a.cfg.ImageDir, MaxImage: uint64(a.cfg.MaxImageMiB) << 20, Paranoid: a.cfg.ParanoidImages,
		AutoAcceptFromVerified: a.cfg.AutoAcceptFromVerified, MDNS: a.cfg.MDNS, History: a.cfg.History}
	listen := ":" + strconv.Itoa(int(a.cfg.ListenPort))
	var torOpts *core.TorOptions
	if a.cfg.Transport == "tor" || a.cfg.Transport == "both" {
		key, err := st.LoadOnionKey()
		if err != nil {
			dialog.ShowError(err, a.win)
			return
		}
		torOpts = &core.TorOptions{Key: key, Port: a.cfg.ListenPort, Exe: a.cfg.TorExe}
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
	go func() { errc <- e.Start(ctx); close(a.done) }()
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
