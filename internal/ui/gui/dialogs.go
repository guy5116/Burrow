//go:build gui

package gui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"net/url"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/ui/common"
)

func parseURL(s string) (*url.URL, error) { return url.Parse(s) }

// showInviteDialog creates an invite and shows it as QR + copyable text.
func (a *App) showInviteDialog() {
	host := widget.NewEntry()
	host.SetText(a.cfg.InviteHost)
	if onion := a.e.OnionAddress(); onion != "" {
		host.SetText(onion)
	}
	host.PlaceHolder = "address peers dial (IP, hostname, .onion)"
	ttl := widget.NewSelect([]string{"15m", "1h", "24h"}, nil)
	ttl.SetSelected("1h")
	multi := widget.NewCheck("Multi-use (up to 64 peers)", nil)
	items := []*widget.FormItem{widget.NewFormItem("Host", host), widget.NewFormItem("Valid for", ttl), widget.NewFormItem("", multi)}
	dialog.ShowForm("Create invite", "Create", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		d, _ := time.ParseDuration(ttl.Selected)
		opts := core.InviteOptions{Host: host.Text, TTL: d, MultiUse: multi.Checked}
		if strings.HasSuffix(host.Text, ".onion") {
			opts.Kind = "tor"
		}
		inv, err := a.e.CreateInvite(opts)
		if err != nil {
			a.errDialog(err)
			return
		}
		text := widget.NewEntry()
		text.SetText(inv.String)
		text.Wrapping = fyne.TextWrapBreak
		text.MultiLine = true
		cp := widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), func() { a.win.Clipboard().SetContent(inv.String) })
		content := container.NewVBox(
			widget.NewLabel(fmt.Sprintf("Expires %s. Share it over a channel you trust and treat it like a password.", inv.Expiry.Local().Format(time.RFC822))),
			qrImage(inv.String), text, cp)
		dialog.ShowCustom("Invite", "Close", content, a.win)
	}, a.win)
}

func qrImage(content string) fyne.CanvasObject {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return widget.NewLabel("(QR unavailable)")
	}
	img := canvas.NewImageFromImage(q.Image(256))
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(256, 256))
	return img
}

// showConnectDialog pastes an invite (password-style field) or picks a contact.
func (a *App) showConnectDialog() {
	inv := widget.NewPasswordEntry()
	inv.PlaceHolder = "burrow1:… (paste the invite)"
	dialog.ShowForm("Connect", "Connect", "Cancel", []*widget.FormItem{widget.NewFormItem("Invite", inv)}, func(ok bool) {
		s := inv.Text
		inv.SetText("")
		if !ok || s == "" {
			return
		}
		info, err := core.DescribeInvite(s)
		if err != nil {
			a.errDialog(err)
			return
		}
		if !info.Hostname {
			a.connectTarget(core.Target{Invite: s})
			return
		}
		a.confirm(dialog.NewConfirm("This invite uses a hostname",
			"Looking up "+info.Host+" tells your DNS resolver which host you are contacting. IP addresses and onion addresses avoid that.\n\nConnect anyway?",
			func(yes bool) {
				if yes {
					a.connectTarget(core.Target{Invite: s})
				}
			}, a.win))
	}, a.win)
}

// showVerifyDialog shows the safety number for the selected contact.
func (a *App) showVerifyDialog() {
	id, ok := a.selected()
	if !ok {
		dialog.ShowInformation("Verify", "Select a contact first.", a.win)
		return
	}
	sn, err := a.e.SafetyNumber(id)
	if err != nil {
		a.errDialog(err)
		return
	}
	c, _ := a.e.Contact(id)
	num := widget.NewLabelWithStyle(common.FormatSafety(sn), fyne.TextAlignCenter, fyne.TextStyle{Monospace: true, Bold: true})
	content := container.NewVBox(
		widget.NewLabel(fmt.Sprintf("Compare these 12 groups with %s over a channel you trust (a call, in person). Both of you must see exactly the same numbers.", c.Nickname)),
		num,
		widget.NewLabel("Your fingerprint: "+a.e.Identity().Display),
		widget.NewLabel("Their fingerprint: "+id.Display()))
	a.confirm(dialog.NewCustomConfirm("Safety number", "Mark as verified", "Not now", content, func(ok bool) {
		if ok {
			a.errDialog(a.e.VerifyContact(id))
		}
	}, a.win))
}

// showSettingsDialog edits config.toml; engine-level values apply on restart.
func (a *App) showSettingsDialog() {
	c := a.cfg
	port := widget.NewEntry()
	port.SetText(strconv.Itoa(int(c.ListenPort)))
	name := widget.NewEntry()
	name.SetText(c.DisplayName)
	host := widget.NewEntry()
	host.SetText(c.InviteHost)
	imgDir := widget.NewEntry()
	imgDir.SetText(c.ImageDir)
	maxImg := widget.NewEntry()
	maxImg.SetText(strconv.Itoa(int(c.MaxImageMiB)))
	maxFile := widget.NewEntry()
	maxFile.SetText(strconv.Itoa(int(c.MaxFileMiB)))
	typing := widget.NewCheck("Typing indicators", nil)
	typing.Checked = c.Typing
	stamps := widget.NewCheck("Send timestamps", nil)
	stamps.Checked = c.Timestamps
	auto := widget.NewCheck("Auto-accept images from verified contacts (files always ask)", nil)
	auto.Checked = c.AutoAcceptFromVerified
	paranoid := widget.NewCheck("Paranoid images (re-encode to PNG)", nil)
	paranoid.Checked = c.ParanoidImages
	mdns := widget.NewCheck("LAN discovery (mDNS, announces presence)", nil)
	mdns.Checked = c.MDNS
	links := widget.NewCheck("Allow opening links in the browser", nil)
	links.Checked = c.OpenLinks
	reconnect := widget.NewCheck("Auto-reconnect to contacts", nil)
	reconnect.Checked = c.AutoReconnect
	transport := widget.NewSelect([]string{"tcp", "tor", "both"}, nil)
	transport.SetSelected(c.Transport)
	history := widget.NewCheck("Keep encrypted message history", nil)
	history.Checked = c.History
	pass := widget.NewButton("Change passphrase…", a.showPassphraseDialog)
	items := []*widget.FormItem{
		widget.NewFormItem("Listen port", port), widget.NewFormItem("Display name", name), widget.NewFormItem("Invite host", host),
		widget.NewFormItem("Download folder", imgDir),
		widget.NewFormItem("Largest image (MiB)", maxImg), widget.NewFormItem("Largest file (MiB, 0 = refuse files)", maxFile),
		widget.NewFormItem("", typing), widget.NewFormItem("", stamps),
		widget.NewFormItem("", auto), widget.NewFormItem("", paranoid), widget.NewFormItem("", mdns), widget.NewFormItem("", links),
		widget.NewFormItem("", reconnect), widget.NewFormItem("Transport", transport), widget.NewFormItem("", history),
		widget.NewFormItem("", pass),
	}
	dialog.ShowForm("Settings", "Save", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		n := c
		for k, v := range map[string]string{"listen_port": port.Text, "display_name": name.Text, "invite_host": host.Text, "image_dir": imgDir.Text,
			"max_image_mib": maxImg.Text, "max_file_mib": maxFile.Text,
			"typing": strconv.FormatBool(typing.Checked), "timestamps": strconv.FormatBool(stamps.Checked),
			"auto_accept_from_verified": strconv.FormatBool(auto.Checked), "paranoid_images": strconv.FormatBool(paranoid.Checked),
			"mdns": strconv.FormatBool(mdns.Checked), "open_links": strconv.FormatBool(links.Checked), "auto_reconnect": strconv.FormatBool(reconnect.Checked),
			"transport": transport.Selected, "history": strconv.FormatBool(history.Checked)} {
			if err := n.Set(k, v); err != nil {
				a.errDialog(err)
				return
			}
		}
		if err := store.SaveConfig(a.paths.Config, n); err != nil {
			a.errDialog(err)
			return
		}
		a.cfg = n
		dialog.ShowInformation("Settings saved", "Port, name, size limits and reconnect changes apply after a restart.", a.win)
	}, a.win)
}

func (a *App) showPassphraseDialog() {
	pw1, pw2 := widget.NewPasswordEntry(), widget.NewPasswordEntry()
	insecure := widget.NewCheck("Remove the passphrase (insecure)", nil)
	items := []*widget.FormItem{widget.NewFormItem("New passphrase", pw1), widget.NewFormItem("Repeat", pw2), widget.NewFormItem("", insecure)}
	dialog.ShowForm("Change passphrase", "Re-encrypt", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		var pw []byte
		if !insecure.Checked {
			if pw1.Text == "" || pw1.Text != pw2.Text {
				a.errDialog(errors.New("passphrases are empty or do not match"))
				return
			}
			pw = []byte(pw1.Text)
		}
		pw1.SetText("")
		pw2.SetText("")
		a.async(func() error { return a.st.ChangePassphrase(pw, store.Options{}) }, func(err error) {
			if err != nil && !errors.Is(err, store.ErrOldStoreRemains) {
				a.errDialog(err)
				return
			}
			dialog.ShowInformation("Done", "The store was re-encrypted.", a.win)
		})
	}, a.win)
}

// showOfferDialog asks about an incoming image or file, size first; Reject is
// the default button.
func (a *App) showOfferDialog(o core.ImageOffered) {
	msg := fmt.Sprintf("%s offers %s.\nNothing is downloaded until you accept.", a.ctl.Names.Nick(o.Peer), common.Offer(o))
	title := "Image offered"
	if o.Format == core.FormatFile {
		title = "File offered"
		msg += "\nBurrow never opens received files. Only open it yourself if you trust the sender."
	}
	if o.Caption != "" {
		msg += fmt.Sprintf("\nCaption: %q", o.Caption)
	}
	label := widget.NewLabel(msg)
	label.Wrapping = fyne.TextWrapWord
	accept := widget.NewButton("Accept", func() {
		if d := a.offers[o.ID]; d != nil {
			d.Hide()
			delete(a.offers, o.ID)
		}
		a.async(func() error { return a.e.AcceptImage(o.ID, "") }, func(err error) {
			if errors.Is(err, core.ErrBusy) {
				a.showOfferDialog(o) // still on offer: ask again, with the reason
			}
			a.errDialog(err)
		})
	})
	accept.Importance = widget.LowImportance
	d := dialog.NewCustom(title, "Reject", container.NewVBox(label, accept), a.win)
	d.SetOnClosed(func() {
		if _, pending := a.offers[o.ID]; pending {
			delete(a.offers, o.ID)
			go func() { _ = a.e.RejectImage(o.ID) }()
		}
	})
	a.offers[o.ID] = d
	d.Show()
}

// pickImage opens a file chooser and offers the chosen image or file.
func (a *App) pickImage() {
	if _, ok := a.selected(); !ok {
		dialog.ShowInformation("No contact selected", "Pick a contact in the sidebar first.", a.win)
		return
	}
	dialog.ShowFileOpen(func(r fyne.URIReadCloser, err error) {
		if err != nil || r == nil {
			return
		}
		p := r.URI().Path()
		_ = r.Close()
		a.offerImage(p)
	}, a.win)
}

func (a *App) offerImage(path string) {
	id, ok := a.selected()
	if !ok {
		return
	}
	a.async(func() error {
		_, err := a.e.SendFile(context.Background(), id, path, "")
		return err
	}, func(err error) {
		if err != nil {
			a.errDialog(err)
			return
		}
		a.addRow(id, &row{kind: rowSystem, text: "sending (images lose their metadata; other files go as they are, without their name)…"})
	})
}

// imageBubble is a tappable thumbnail; decoding happens off the UI thread.
func (a *App) imageBubble(r *row) fyne.CanvasObject {
	img := canvas.NewImageFromImage(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(256, 192))
	path := r.path
	go func() {
		th, err := core.DecodeImage(context.Background(), path, 512)
		fyne.Do(func() {
			if err != nil {
				img.Hide()
				return
			}
			img.Image = th
			img.Refresh()
		})
	}()
	return container.NewVBox(newTappable(img, func() { a.showViewer(path) }), widget.NewLabel(path))
}

// tappable makes any canvas object clickable (used for image thumbnails).
type tappable struct {
	widget.BaseWidget
	content fyne.CanvasObject
	onTap   func()
}

func newTappable(content fyne.CanvasObject, onTap func()) *tappable {
	t := &tappable{content: content, onTap: onTap}
	t.ExtendBaseWidget(t)
	return t
}

// CreateRenderer shows the wrapped object.
func (t *tappable) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(t.content) }

// Tapped opens the viewer.
func (t *tappable) Tapped(*fyne.PointEvent) {
	if t.onTap != nil {
		t.onTap()
	}
}

// showViewer opens a window with the full image and a fit / 1:1 toggle.
func (a *App) showViewer(path string) {
	w := a.fa.NewWindow("Image")
	w.Resize(fyne.NewSize(800, 600))
	img := canvas.NewImageFromImage(image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	img.FillMode = canvas.ImageFillContain
	scroll := container.NewScroll(img)
	fit := true
	var full image.Image
	toggle := widget.NewButton("1:1", nil)
	toggle.OnTapped = func() {
		fit = !fit
		if fit {
			img.FillMode = canvas.ImageFillContain
			img.SetMinSize(fyne.NewSize(0, 0))
			toggle.SetText("1:1")
		} else if full != nil {
			img.FillMode = canvas.ImageFillOriginal
			b := full.Bounds()
			img.SetMinSize(fyne.NewSize(float32(b.Dx()), float32(b.Dy())))
			toggle.SetText("Fit")
		}
		img.Refresh()
		scroll.Refresh()
	}
	w.SetContent(container.NewBorder(container.NewHBox(toggle, widget.NewLabel(path)), nil, nil, nil, scroll))
	go func() {
		decoded, err := core.DecodeImage(context.Background(), path, 0)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			full = decoded
			img.Image = decoded
			img.Refresh()
		})
	}()
	w.Show()
}
