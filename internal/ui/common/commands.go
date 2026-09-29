package common

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guy5116/burrow/internal/core"
	"github.com/guy5116/burrow/internal/identity"
)

// Controller executes slash commands against the engine for both UIs.
type Controller struct {
	E          *core.Engine
	Names      Names
	Current    *core.PeerID // selected conversation
	InviteHost string       // default host for /invite
	Onion      string       // our onion address when Tor is running
	Typing     bool
	// QR renders text as a terminal QR code; set by the CLI (nil → /invite qr is unavailable).
	QR func(string) string

	mu      sync.Mutex
	nextNum int
	offers  map[int]core.TransferID // short numbers for /accept, /reject, /cancel
	numOf   map[core.TransferID]int
	images  []string // saved image paths, for /view
}

// NewController wires a controller to an engine.
func NewController(e *core.Engine, inviteHost string) *Controller {
	return &Controller{E: e, Names: Names{E: e}, InviteHost: inviteHost, offers: map[int]core.TransferID{}, numOf: map[core.TransferID]int{}}
}

// Observe tracks transfer events so commands can refer to them by number.
// Call it for every engine event before rendering it.
func (c *Controller) Observe(ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e := ev.(type) {
	case core.ImageOffered:
		c.nextNum++
		c.offers[c.nextNum] = e.ID
		c.numOf[e.ID] = c.nextNum
	case core.TransferDone:
		if !e.Peer.Outgoing {
			c.images = append(c.images, e.Path)
		}
		c.forget(e.ID)
	case core.TransferFailed:
		c.forget(e.ID)
	}
}

func (c *Controller) forget(id core.TransferID) {
	if n, ok := c.numOf[id]; ok {
		delete(c.offers, n)
		delete(c.numOf, id)
	}
}

// Number returns the short number for an offered transfer (0 if unknown).
func (c *Controller) Number(id core.TransferID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.numOf[id]
}

// Images returns the paths of received images in arrival order.
func (c *Controller) Images() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.images...)
}

func (c *Controller) transferByNum(arg string) (core.TransferID, error) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return core.TransferID{}, errors.New("usage: <command> <transfer number> (see /transfers)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.offers[n]
	if !ok {
		return core.TransferID{}, errors.New("no pending transfer with that number")
	}
	return id, nil
}

// Resolve finds a contact by exact nickname, or by fingerprint prefix (≥ 8 chars).
func (c *Controller) Resolve(name string) (core.PeerID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return core.PeerID{}, errors.New("no contact given")
	}
	if id, err := identity.ParseFingerprint(name); err == nil {
		return id, nil
	}
	var byNick, byPrefix []core.Contact
	lower := strings.ToLower(strings.ReplaceAll(name, " ", ""))
	for _, ct := range c.E.Contacts() {
		if ct.Nickname == name {
			byNick = append(byNick, ct)
		}
		if len(lower) >= 8 && strings.HasPrefix(ct.ID.Fingerprint(), lower) {
			byPrefix = append(byPrefix, ct)
		}
	}
	switch {
	case len(byNick) == 1:
		return byNick[0].ID, nil
	case len(byNick) > 1:
		return core.PeerID{}, fmt.Errorf("%d contacts are named %q; use a fingerprint prefix", len(byNick), name)
	case len(byPrefix) == 1:
		return byPrefix[0].ID, nil
	case len(byPrefix) > 1:
		return core.PeerID{}, errors.New("fingerprint prefix is ambiguous")
	}
	return core.PeerID{}, core.ErrUnknownContact
}

// Help lists the commands.
const Help = `/connect <invite|contact>   connect (an invite string starts with burrow1:)
/to <contact>               select the conversation for plain text lines
/msg <contact> <text>       send to a specific contact
/contacts                   list contacts (✓ verified, * online)
/invite [multi] [tor] [qr] [host] create an invite (single-use, 1 h; tor = onion, qr = also as QR code)
/safety <contact>           show the safety number to compare out of band
/verify <contact>           mark verified after comparing safety numbers
/rename <contact> <name>    set a nickname
/block|/unblock <contact>   block or unblock
/remove <contact>           delete a contact
/disconnect <contact>       close the session
/image <path> [caption]     send an image (metadata is stripped first)
/accept <n> | /reject <n>   answer an image offer by its number
/cancel <n>                 cancel a transfer
/transfers                  list pending offers
/partials                   list partial downloads that can resume
/view <n>                   show a received image inline (Kitty/iTerm2), n from /images
/images                     list received images
/history [n]                show the last n stored messages (needs history = true)
/typing on|off              typing indicators (off by default)
/id                         show your fingerprint
/quit                       exit`

// Exec runs one input line. Lines without a leading slash are messages to the
// current contact. It returns output lines and whether the UI should quit.
func (c *Controller) Exec(ctx context.Context, line string) (out []string, quit bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, false
	}
	if !strings.HasPrefix(line, "/") {
		if c.Current == nil {
			return []string{"! no conversation selected; use /to <contact>"}, false
		}
		return c.send(*c.Current, line), false
	}
	cmd, rest, _ := strings.Cut(line[1:], " ")
	rest = strings.TrimSpace(rest)
	fail := func(err error) ([]string, bool) { return []string{"! " + err.Error()}, false }
	switch cmd {
	case "quit", "q", "exit":
		return nil, true
	case "help", "?":
		return strings.Split(Help, "\n"), false
	case "id":
		id := c.E.Identity()
		out = []string{"your fingerprint: " + id.Display}
		if c.Onion != "" {
			out = append(out, "your onion address: "+c.Onion)
		}
		return out, false
	case "history":
		if c.Current == nil {
			return fail(errors.New("no conversation selected; use /to <contact>"))
		}
		n, _ := strconv.Atoi(rest)
		hist, err := c.E.History(*c.Current, n)
		if err != nil {
			return fail(err)
		}
		for _, h := range hist {
			who := c.Names.Nick(h.Peer)
			if h.Mine {
				who = "me"
			}
			out = append(out, fmt.Sprintf("%s <%s> %s", h.At.Local().Format("2006-01-02 15:04"), who, h.Text))
		}
		if len(out) == 0 {
			out = []string{"(no stored messages)"}
		}
		return out, false
	case "connect":
		if rest == "" {
			return fail(errors.New("usage: /connect <invite|contact>"))
		}
		var target core.Target
		if strings.HasPrefix(rest, "burrow1:") {
			target.Invite = rest
			if info, err := core.DescribeInvite(rest); err == nil && info.Hostname {
				out = append(out, "note: this invite uses the hostname "+info.Host+"; looking it up tells your DNS resolver which host you are contacting")
			}
		} else {
			id, err := c.Resolve(rest)
			if err != nil {
				return fail(err)
			}
			target.Contact = &id
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		id, err := c.E.Connect(cctx, target)
		if err != nil {
			return append(out, "! "+err.Error()), false
		}
		c.Current = &id
		return append(out, "* connected to "+c.Names.Label(id)+"; now talking to them"), false
	case "to":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		c.Current = &id
		return []string{"* now talking to " + c.Names.Label(id)}, false
	case "msg":
		who, text, _ := strings.Cut(rest, " ")
		id, err := c.Resolve(who)
		if err != nil {
			return fail(err)
		}
		return c.send(id, strings.TrimSpace(text)), false
	case "contacts":
		cs := c.E.Contacts()
		sort.Slice(cs, func(i, j int) bool { return cs[i].Nickname < cs[j].Nickname })
		if len(cs) == 0 {
			return []string{"(no contacts)"}, false
		}
		for _, ct := range cs {
			flags := " "
			if ct.Online {
				flags = "*"
			}
			v := "unverified"
			if ct.Verified {
				v = "verified ✓"
			}
			if ct.Blocked {
				v += ", BLOCKED"
			}
			out = append(out, fmt.Sprintf("%s %-20s %s  %s", flags, ct.Nickname, ct.ID.Display(), v))
		}
		return out, false
	case "invite":
		opts := core.InviteOptions{Host: c.InviteHost}
		wantQR := false
		for _, f := range strings.Fields(rest) {
			switch f {
			case "multi", "--multi-use":
				opts.MultiUse = true
			case "qr", "--qr":
				wantQR = true
			case "tor":
				if c.Onion == "" {
					return fail(errors.New("tor is not running (config: transport = tor or both)"))
				}
				opts.Host, opts.Kind = c.Onion, "tor"
			default:
				opts.Host = f
			}
		}
		inv, err := c.E.CreateInvite(opts)
		if err != nil {
			return fail(err)
		}
		kind := "single-use"
		if inv.MultiUse {
			kind = "multi-use"
		}
		out = []string{fmt.Sprintf("invite (%s, expires %s) — share it over a channel you trust, treat it like a password:", kind, inv.Expiry.Local().Format(time.RFC822)), inv.String}
		if wantQR {
			if c.QR == nil {
				out = append(out, "! QR codes are not available here")
			} else {
				out = append(out, strings.Split(strings.TrimRight(c.QR(inv.String), "\n"), "\n")...)
			}
		}
		return out, false
	case "safety":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		sn, err := c.E.SafetyNumber(id)
		if err != nil {
			return fail(err)
		}
		return append([]string{"safety number with " + c.Names.Label(id) + " (both of you must see the same 12 groups):"},
			strings.Split(FormatSafety(sn), "\n")...), false
	case "verify":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		if err := c.E.VerifyContact(id); err != nil {
			return fail(err)
		}
		return []string{"* " + c.Names.Label(id) + " marked verified"}, false
	case "rename":
		who, name, _ := strings.Cut(rest, " ")
		id, err := c.Resolve(who)
		if err != nil {
			return fail(err)
		}
		if err := c.E.RenameContact(id, name); err != nil {
			return fail(err)
		}
		return []string{"* renamed to " + c.Names.Label(id)}, false
	case "block", "unblock":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		if err := c.E.BlockContact(id, cmd == "block"); err != nil {
			return fail(err)
		}
		return []string{"* " + cmd + "ed " + c.Names.Label(id)}, false
	case "remove":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		label := c.Names.Label(id)
		if err := c.E.RemoveContact(id); err != nil {
			return fail(err)
		}
		if c.Current != nil && *c.Current == id {
			c.Current = nil
		}
		return []string{"* removed " + label}, false
	case "disconnect":
		id, err := c.Resolve(rest)
		if err != nil {
			return fail(err)
		}
		if err := c.E.Disconnect(id); err != nil {
			return fail(err)
		}
		return []string{"* disconnecting from " + c.Names.Label(id)}, false
	case "image":
		if c.Current == nil {
			return fail(errors.New("no conversation selected; use /to <contact>"))
		}
		path, caption, _ := strings.Cut(rest, " ")
		if path == "" {
			return fail(errors.New("usage: /image <path> [caption]"))
		}
		id, err := c.E.SendImage(ctx, *c.Current, path, strings.TrimSpace(caption))
		if err != nil {
			return fail(err)
		}
		return []string{"* preparing image " + hexID(id)[:8] + " (stripping metadata)…"}, false
	case "accept", "reject", "cancel":
		id, err := c.transferByNum(rest)
		if err != nil {
			return fail(err)
		}
		switch cmd {
		case "accept":
			err = c.E.AcceptImage(id, "")
		case "reject":
			err = c.E.RejectImage(id)
		default:
			err = c.E.CancelTransfer(id)
		}
		if err != nil {
			return fail(err)
		}
		return []string{"* " + cmd + "ed transfer " + rest}, false
	case "transfers":
		c.mu.Lock()
		nums := make([]int, 0, len(c.offers))
		for n := range c.offers {
			nums = append(nums, n)
		}
		c.mu.Unlock()
		sort.Ints(nums)
		if len(nums) == 0 {
			return []string{"(no pending offers)"}, false
		}
		for _, n := range nums {
			out = append(out, fmt.Sprintf("#%d %s", n, hexID(c.offers[n])[:8]))
		}
		return out, false
	case "partials":
		return c.StartupNotes(true), false
	case "images":
		imgs := c.Images()
		if len(imgs) == 0 {
			return []string{"(no images received yet)"}, false
		}
		for i, p := range imgs {
			out = append(out, fmt.Sprintf("#%d %s", i+1, p))
		}
		return out, false
	case "view":
		return []string{"! /view is only available in the full-screen chat"}, false
	case "typing":
		c.Typing = rest == "on"
		return []string{"* typing indicators " + map[bool]string{true: "on", false: "off"}[c.Typing]}, false
	}
	return fail(fmt.Errorf("unknown command /%s (try /help)", cmd))
}

func (c *Controller) send(id core.PeerID, text string) []string {
	mid, err := c.E.SendText(id, text)
	if err != nil {
		return []string{"! " + err.Error()}
	}
	return []string{fmt.Sprintf("* queued %016x to %s", uint64(mid), c.Names.Nick(id))}
}

// StartupNotes lists partial downloads that can resume. With always set it
// reports an empty list too (the /partials command); at startup it stays quiet.
func (c *Controller) StartupNotes(always bool) []string {
	parts := c.E.Resumable()
	if len(parts) == 0 {
		if always {
			return []string{"(no partial downloads)"}
		}
		return nil
	}
	out := []string{fmt.Sprintf("* %d partial download(s) will resume when the sender offers the same image again:", len(parts))}
	for _, p := range parts {
		out = append(out, fmt.Sprintf("  from %s: %s of %s %s", c.Names.Nick(p.Peer), Size(p.Done), Size(p.Size), FormatName(p.Format)))
	}
	return out
}

// Status renders the status-bar text for the selected contact: short
// fingerprint, verification state, presence and transport kind.
func (c *Controller) Status() string {
	s := "me " + c.E.Identity().ID.Short()
	if c.Current == nil {
		return s
	}
	ct, err := c.E.Contact(*c.Current)
	if err != nil {
		return s
	}
	ver := "UNVERIFIED"
	if ct.Verified {
		ver = "verified"
	}
	on := "offline"
	if ct.Online {
		on = "online via " + string(ct.Transport)
	}
	return s + fmt.Sprintf(" · talking to %s (%s, %s, %s)", ct.Nickname, ct.ID.Short(), ver, on)
}

// ConnectInvite connects with an invite held in bytes (from a no-echo prompt
// or a pipe). The bytes are wiped by the engine.
func (c *Controller) ConnectInvite(ctx context.Context, inv []byte) []string {
	var out []string
	if info, err := core.DescribeInviteBytes(inv); err == nil && info.Hostname {
		out = append(out, "note: this invite uses the hostname "+info.Host+"; looking it up tells your DNS resolver which host you are contacting")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := c.E.Connect(cctx, core.Target{InviteBytes: inv})
	if err != nil {
		return append(out, "! "+err.Error())
	}
	c.Current = &id
	return append(out, "* connected to "+c.Names.Label(id)+"; now talking to them")
}
