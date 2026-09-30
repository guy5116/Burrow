//go:build gui

package gui

import (
	"errors"
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

const insecureWarning = "Without a passphrase, anyone who can read your data directory can read your identity, contacts and invites."

// showUnlock decides between the identity wizard, the passphrase prompt and a
// direct open, and renders it as the window content.
func (a *App) showUnlock() {
	mode, err := store.Mode(a.paths.Data)
	switch {
	case errors.Is(err, store.ErrNoStore):
		a.win.SetContent(a.wizard())
	case errors.Is(err, store.ErrInUse):
		a.win.SetContent(container.NewCenter(widget.NewLabel("Another Burrow process holds this identity. Close it and start again.")))
		a.showInUse()
	case err != nil:
		a.win.SetContent(container.NewCenter(widget.NewLabel("Cannot read the store: " + err.Error())))
	case mode == store.ModeNone:
		a.open(nil)
	default:
		a.win.SetContent(a.unlockForm())
	}
}

// open unlocks the store with pw (nil for a no-passphrase store) and starts
// the engine. Deriving the key takes a moment and runs off the Fyne thread.
func (a *App) open(pw []byte) {
	var st *store.Store
	a.async(func() (err error) {
		st, err = store.Open(a.paths.Data, pw)
		return err
	}, func(err error) {
		switch {
		case errors.Is(err, store.ErrInUse):
			a.showInUse()
		case errors.Is(err, store.ErrBlob):
			dialog.ShowError(errors.New("wrong passphrase or corrupted store"), a.win)
		case err != nil:
			dialog.ShowError(err, a.win)
		default:
			a.startEngine(st)
		}
	})
}

func (a *App) showInUse() {
	dialog.ShowInformation("Store in use", "Another Burrow process (perhaps the CLI) holds this identity. Close it first.", a.win)
}

func (a *App) unlockForm() fyne.CanvasObject {
	pw := widget.NewPasswordEntry()
	pw.PlaceHolder = "Passphrase"
	status := widget.NewLabel("")
	submit := func(string) {
		if pw.Text == "" {
			status.SetText("Enter your passphrase.")
			return
		}
		b := []byte(pw.Text)
		pw.SetText("")
		a.open(b)
	}
	pw.OnSubmitted = submit
	btn := widget.NewButton("Unlock", func() { submit("") })
	btn.Importance = widget.HighImportance
	form := container.NewVBox(widget.NewLabelWithStyle("Unlock Burrow", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}), pw, btn, status)
	a.win.Canvas().Focus(pw)
	return container.NewCenter(container.NewGridWrap(fyne.NewSize(360, 180), form))
}

// wizard creates a new identity.
func (a *App) wizard() fyne.CanvasObject {
	pw1, pw2 := widget.NewPasswordEntry(), widget.NewPasswordEntry()
	pw1.PlaceHolder, pw2.PlaceHolder = "Choose a passphrase", "Repeat the passphrase"
	insecure := widget.NewCheck("No passphrase (insecure)", nil)
	status := widget.NewLabel("")
	status.Wrapping = fyne.TextWrapWord
	create := widget.NewButton("Create identity", func() {
		var pw []byte
		if insecure.Checked {
			pw = nil
		} else {
			if pw1.Text == "" {
				status.SetText("A passphrase is required (or tick the insecure option).")
				return
			}
			if pw1.Text != pw2.Text {
				status.SetText("The passphrases do not match.")
				return
			}
			pw = []byte(pw1.Text)
		}
		pw1.SetText("")
		pw2.SetText("")
		status.SetText("Creating your identity…")
		var st *store.Store
		a.async(func() (err error) {
			if st, err = store.Init(a.paths.Data, pw, store.Options{}); err != nil {
				return err
			}
			if err = store.SaveConfig(a.paths.Config, a.cfg); err != nil {
				st.Close()
			}
			return err
		}, func(err error) {
			if err != nil {
				status.SetText(err.Error())
				return
			}
			a.startEngine(st)
		})
	})
	create.Importance = widget.HighImportance
	insecure.OnChanged = func(on bool) {
		if on {
			status.SetText(insecureWarning)
		} else {
			status.SetText("")
		}
	}
	ip := widget.NewLabel(common.IPWarning)
	ip.Wrapping = fyne.TextWrapWord
	form := container.NewVBox(
		widget.NewLabelWithStyle("Welcome to Burrow", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel(fmt.Sprintf("Your identity will be stored encrypted in\n%s", a.paths.Data)),
		pw1, pw2, insecure, create, status, ip)
	return container.NewCenter(container.NewGridWrap(fyne.NewSize(520, 520), form))
}
