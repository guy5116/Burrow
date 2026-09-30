//go:build gui

package gui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/ui/common"
)

// buildMain lays out sidebar + conversation + composer + status bar.
func (a *App) buildMain() {
	a.refreshContacts()
	a.list = widget.NewList(
		func() int { return len(a.contacts) },
		func() fyne.CanvasObject { return widget.NewLabel("contact") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			if i >= len(a.contacts) {
				return
			}
			// Presence and verification come first, in columns of their own:
			// whatever a contact called itself cannot reach them.
			c := a.contacts[i]
			mark, v := "○", "?"
			if c.Online {
				mark = "●"
			}
			if c.Verified {
				v = "✓"
			}
			o.(*widget.Label).SetText(mark + v + " " + c.Nickname)
		})
	a.list.OnSelected = func(i widget.ListItemID) {
		if i < len(a.contacts) {
			id := a.contacts[i].ID
			a.choose(id)
			a.renderConversation()
			a.updateStatus()
			a.loadHistory(id)
		}
	}
	toolbar := container.NewHBox(
		widget.NewButtonWithIcon("Invite", theme.ContentAddIcon(), a.showInviteDialog),
		widget.NewButtonWithIcon("Connect", theme.MailSendIcon(), a.showConnectDialog),
		widget.NewButtonWithIcon("Verify", theme.ConfirmIcon(), a.showVerifyDialog),
		widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), a.showSettingsDialog),
	)
	a.conv = container.NewVBox()
	a.scroll = container.NewVScroll(a.conv)
	a.composer = widget.NewMultiLineEntry()
	a.composer.PlaceHolder = "Message (Enter to send, Shift+Enter for a new line)"
	a.composer.Wrapping = fyne.TextWrapWord
	a.composer.OnSubmitted = func(s string) { a.sendText(s) }
	a.composer.OnChanged = func(s string) { a.setTyping(s != "") }
	send := widget.NewButtonWithIcon("Send", theme.MailSendIcon(), func() { a.sendText(a.composer.Text) })
	send.Importance = widget.HighImportance
	imgBtn := widget.NewButtonWithIcon("File", theme.FileIcon(), a.pickImage)
	bottom := container.NewBorder(nil, nil, nil, container.NewHBox(imgBtn, send), a.composer)
	a.status = widget.NewLabel("")
	a.updateStatus()
	side := container.NewBorder(toolbar, nil, nil, nil, a.list)
	right := container.NewBorder(nil, container.NewVBox(bottom, a.status), nil, nil, a.scroll)
	split := container.NewHSplit(side, right)
	split.SetOffset(0.28)
	a.win.SetContent(split)
	a.win.SetOnDropped(func(_ fyne.Position, uris []fyne.URI) {
		for _, u := range uris {
			if u.Scheme() == "file" {
				a.offerImage(u.Path())
			}
		}
	})
	a.win.Canvas().Focus(a.composer)
	if notes := a.ctl.StartupNotes(false); len(notes) > 0 {
		dialog.ShowInformation("Partial downloads", strings.Join(notes, "\n"), a.win)
	}
}

func (a *App) refreshContacts() {
	a.contacts = a.e.Contacts()
	sort.Slice(a.contacts, func(i, j int) bool { return a.contacts[i].Nickname < a.contacts[j].Nickname })
	if a.list != nil {
		a.list.Refresh()
	}
}

func (a *App) updateStatus() {
	if a.status == nil {
		return
	}
	s := a.ctl.Status()
	a.status.SetText(s)
}

func (a *App) sendText(s string) {
	id, ok := a.selected()
	if !ok {
		dialog.ShowInformation("No contact selected", "Pick a contact in the sidebar first.", a.win)
		return
	}
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return
	}
	mid, err := a.e.SendText(id, s)
	if err != nil {
		a.errDialog(err)
		return
	}
	a.composer.SetText("") // OnChanged reports that the typing stopped
	r := &row{kind: rowText, text: s, mine: true, status: core.StatusPending, id: mid}
	a.byMsg[mid] = r
	a.addRow(id, r)
}

// addRow appends a row to a conversation and shows it when the conversation
// is selected. Beyond maxRows the oldest rows are dropped.
func (a *App) addRow(id core.PeerID, r *row) {
	rows := append(a.msgs[id], r)
	dropped := len(rows) - maxRows
	if dropped > 0 {
		for _, old := range rows[:dropped] {
			delete(a.byMsg, old.id)
		}
		rows = append(rows[:0], rows[dropped:]...)
	}
	a.msgs[id] = rows
	sel, ok := a.selected()
	switch {
	case !ok || sel != id:
	case dropped > 0:
		a.renderConversation()
	default:
		a.conv.Add(a.rowWidget(r))
		a.conv.Refresh()
		a.scroll.ScrollToBottom()
	}
}

func (a *App) renderConversation() {
	a.conv.RemoveAll()
	if id, ok := a.selected(); ok {
		for _, r := range a.msgs[id] {
			a.conv.Add(a.rowWidget(r))
		}
	}
	a.conv.Refresh()
	a.scroll.ScrollToBottom()
}

// rowWidget renders a row. Text is plain: no markdown, no auto-linking.
func (a *App) rowWidget(r *row) fyne.CanvasObject {
	if r.widget != nil {
		return r.widget
	}
	switch r.kind {
	case rowSystem:
		l := widget.NewLabel(r.text)
		l.Wrapping = fyne.TextWrapWord
		l.TextStyle = fyne.TextStyle{Italic: true}
		r.widget = l
	case rowImage:
		r.widget = a.imageBubble(r)
	default:
		// Who said it stands in a widget of its own, and the lines of a
		// message after the first carry a gutter: a message with line breaks
		// cannot pass for several messages.
		who := widget.NewLabelWithStyle("them", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		if r.mine {
			who.SetText("me")
		}
		l := widget.NewLabel(common.Body(r.text))
		l.Wrapping = fyne.TextWrapWord
		l.Selectable = true
		items := []fyne.CanvasObject{who, l}
		for _, u := range links(r.text) {
			items = append(items, a.linkRow(u))
		}
		if r.mine {
			st := widget.NewLabel(statusMark(r.status))
			items = append(items, st)
		}
		r.widget = container.NewVBox(items...)
	}
	return r.widget
}

func statusMark(s core.Status) string {
	switch s {
	case core.StatusPending:
		return "… queued"
	case core.StatusSent:
		return "✓ sent"
	case core.StatusDelivered:
		return "✓✓ delivered"
	}
	return "✗ failed"
}

// linkRow shows a detected URL as text with "Copy link" and, only when the
// open_links setting is on, "Open in browser" behind a confirmation.
func (a *App) linkRow(u string) fyne.CanvasObject {
	copyBtn := widget.NewButtonWithIcon("Copy link", theme.ContentCopyIcon(), func() { a.win.Clipboard().SetContent(u) })
	items := []fyne.CanvasObject{copyBtn}
	if a.cfg.OpenLinks {
		items = append(items, widget.NewButton("Open in browser…", func() {
			a.confirm(dialog.NewConfirm("Open this link in your browser?", u, func(ok bool) {
				if ok {
					if parsed, err := parseURL(u); err == nil {
						_ = a.fa.OpenURL(parsed)
					}
				}
			}, a.win))
		}))
	}
	return container.NewHBox(items...)
}

// apply updates the model from one engine event (Fyne thread).
func (a *App) apply(ev core.Event) {
	line := a.ctl.Names.Line(ev)
	switch e := ev.(type) {
	case core.PeerConnected, core.PeerDisconnected, core.Reconnecting, core.PeerVerified:
		a.refreshContacts()
		a.updateStatus()
		if p, ok := peerOf(ev); ok {
			a.addRow(p, &row{kind: rowSystem, text: line})
		}
	case core.NewPeerViaInvite:
		a.refreshContacts()
		if a.sel == nil {
			a.choose(e.Peer)
			a.renderConversation()
		}
		c, _ := a.e.Contact(e.Peer)
		dialog.ShowInformation("New contact",
			fmt.Sprintf("%s connected via an invite.\n\nFingerprint:\n%s\n\nThey are UNVERIFIED until you compare safety numbers (Verify).", c.Nickname, e.Peer.Display()), a.win)
	case core.NameCollision:
		dialog.ShowInformation("Name collision — check fingerprints",
			fmt.Sprintf("A new contact uses the name %q like an existing contact. Someone may be impersonating.\n\nNew:      %s\nExisting: %s", e.Name, e.New.Display(), e.Existing.Display()), a.win)
		a.refreshContacts()
	case core.HandshakeFailed:
		who := "peer"
		if e.Peer != (core.PeerID{}) {
			who = a.label(e.Peer)
		}
		dialog.ShowInformation("Could not connect", fmt.Sprintf("Could not establish a secure session with %s: %s", who, e.Reason), a.win)
	case core.MessageReceived:
		a.addRow(e.Peer, &row{kind: rowText, text: e.Text})
	case core.MessageStatus:
		if r := a.byMsg[e.ID]; r != nil {
			r.status = e.Status
			r.widget = nil
			if e.Status == core.StatusDelivered || e.Status == core.StatusFailed {
				delete(a.byMsg, e.ID) // final: no further status will arrive
			}
			if sel, ok := a.selected(); ok && sel == e.Peer {
				a.renderConversation()
			}
		}
	case core.Typing:
		if e.Typing {
			a.status.SetText(a.ctl.Names.Nick(e.Peer) + " is typing…")
		} else {
			a.updateStatus()
		}
	case core.ImageOffered:
		a.showOfferDialog(e)
	case core.TransferResumed:
		// progress continues from the checkpoint; nothing to show
	case core.TransferProgress:
		if !e.Peer.Outgoing {
			a.status.SetText(fmt.Sprintf("receiving: %d%% of %s", e.Done*100/max(e.Size, 1), common.Size(e.Size)))
		}
	case core.TransferDone:
		a.updateStatus()
		switch {
		case e.Peer.Outgoing:
			a.addRow(e.Peer.Peer, &row{kind: rowSystem, text: common.Kind(e.Image) + " delivered"})
		case e.Image:
			a.addRow(e.Peer.Peer, &row{kind: rowImage, path: e.Path})
		default: // never displayed, never opened
			a.addRow(e.Peer.Peer, &row{kind: rowSystem, text: "file saved: " + e.Path})
		}
	case core.TransferFailed:
		a.updateStatus()
		if d := a.offers[e.ID]; d != nil {
			d.Hide()
			delete(a.offers, e.ID)
		}
		a.addRow(e.Peer.Peer, &row{kind: rowSystem, text: line})
	case core.ErrorEvent:
		dialog.ShowError(fmt.Errorf("%s", e.Message), a.win)
	}
}

func peerOf(ev core.Event) (core.PeerID, bool) {
	switch e := ev.(type) {
	case core.PeerConnected:
		return e.Peer, true
	case core.PeerDisconnected:
		return e.Peer, true
	case core.Reconnecting:
		return e.Peer, true
	case core.PeerVerified:
		return e.Peer, true
	}
	return core.PeerID{}, false
}

func (a *App) connectTarget(t core.Target) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		id, err := a.e.Connect(ctx, t)
		fyne.Do(func() {
			if err != nil {
				a.errDialog(err)
				return
			}
			a.choose(id)
			a.refreshContacts()
			a.renderConversation()
			a.updateStatus()
		})
	}()
}

// loadHistory prefills a conversation from encrypted history (once, when
// enabled). Reading it may wait for the disk, so it happens off the Fyne thread.
func (a *App) loadHistory(id core.PeerID) {
	if !a.cfg.History || len(a.msgs[id]) > 0 {
		return
	}
	var hist []core.HistoryEntry
	a.async(func() (err error) {
		hist, err = a.e.History(id, 200)
		return err
	}, func(err error) {
		if err != nil || len(a.msgs[id]) > 0 {
			return
		}
		for _, h := range hist {
			a.msgs[id] = append(a.msgs[id], &row{kind: rowText, text: h.Text, mine: h.Mine, status: core.StatusDelivered})
		}
		if sel, ok := a.selected(); ok && sel == id {
			a.renderConversation()
		}
	})
}
