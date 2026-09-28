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
)

const insecureWarning = "Without a passphrase, anyone who can read your data directory can read your identity, contacts and invites."

// showUnlock decides between the identity wizard, the passphrase prompt and a
// direct open, and renders it as the window content.
func (a *App) showUnlock() {
	mode, err := store.Mode(a.paths.Data)
	switch {
	case errors.Is(err, store.ErrNoStore):
		a.win.SetContent(a.wizard())
	case err != nil:
		a.win.SetContent(container.NewCenter(widget.NewLabel("Cannot read the store: " + err.Error())))
	case mode == store.ModeNone:
		a.open(nil)
	default:
		a.win.SetContent(a.unlockForm())
	}
}

// open unlocks the store with pw (nil for a no-passphrase store) and starts the engine.
func (a *App) open(pw []byte) {
	st, err := store.Open(a.paths.Data, pw)
	switch {
	case errors.Is(err, store.ErrInUse):
		dialog.ShowInformation("Store in use", "Another Burrow process (perhaps the CLI) holds this identity. Close it first.", a.win)
		return
	case errors.Is(err, store.ErrBlob):
		dialog.ShowError(errors.New("wrong passphrase or corrupted store"), a.win)
		return
	case err != nil:
		dialog.ShowError(err, a.win)
		return
	}
	a.startEngine(st)
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
		st, err := store.Init(a.paths.Data, pw, store.Options{})
		if err != nil {
			status.SetText(err.Error())
			return
		}
		if err := store.SaveConfig(a.paths.Config, a.cfg); err != nil {
			status.SetText(err.Error())
			st.Close()
			return
		}
		a.startEngine(st)
	})
	create.Importance = widget.HighImportance
	insecure.OnChanged = func(on bool) {
		if on {
			status.SetText(insecureWarning)
		} else {
			status.SetText("")
		}
	}
	form := container.NewVBox(
		widget.NewLabelWithStyle("Welcome to Burrow", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
		widget.NewLabel(fmt.Sprintf("Your identity will be stored encrypted in\n%s", a.paths.Data)),
		pw1, pw2, insecure, create, status)
	return container.NewCenter(container.NewGridWrap(fyne.NewSize(420, 320), form))
}
