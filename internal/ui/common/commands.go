package common

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/guy5116/burrow/internal/core"
)

// Controller executes slash commands against the engine for both UIs. It may
// be used from several goroutines.
type Controller struct {
	E          *core.Engine
	Names      Names
	InviteHost string // default host for /invite
	Onion      string // our onion address when Tor is running
	// QR renders text as a terminal QR code; set by the CLI (nil → /invite qr is unavailable).
	QR func(string) string

	mu       sync.Mutex
	current  *core.PeerID // selected conversation
	selected int          // counts selections, so a slow command can tell that the user moved on
	typing   bool
	nextNum  int
	offers   map[int]core.TransferID    // short numbers for /accept, /reject, /cancel
	about    map[core.TransferID]string // what each numbered transfer is, size first
	numOf    map[core.TransferID]int
	ended    map[core.TransferID]bool // transfers that ended before they were numbered
	images   []string                 // saved image paths, for /view
}

// NewController wires a controller to an engine.
func NewController(e *core.Engine, inviteHost string) *Controller {
	return &Controller{E: e, Names: Names{E: e}, InviteHost: inviteHost, typing: e.TypingEnabled(),
		offers: map[int]core.TransferID{}, about: map[core.TransferID]string{}, numOf: map[core.TransferID]int{},
		ended: map[core.TransferID]bool{}}
}

// Selected returns the contact whose conversation is selected.
func (c *Controller) Selected() (core.PeerID, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil {
		return core.PeerID{}, false
	}
	return *c.current, true
}

// Select makes id's conversation the selected one.
func (c *Controller) Select(id core.PeerID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = &id
	c.selected++
}

// Deselect leaves no conversation selected.
func (c *Controller) Deselect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = nil
	c.selected++
}

// selectAfter selects id unless the selection changed since the command that
// asks for it began (selections counted then: since). A connect can take half
// a minute; by then the user may be typing to someone else.
func (c *Controller) selectAfter(since int, id core.PeerID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selected != since {
		return false
	}
	c.current = &id
	c.selected++
	return true
}

func (c *Controller) selections() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selected
}

// TypingOn reports whether typing indicators are to be sent.
func (c *Controller) TypingOn() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.typing
}

// track gives a transfer a short number for /accept, /reject and /cancel,
// unless it has ended already (a send can fail before SendFile returns).
// Caller holds mu.
func (c *Controller) track(id core.TransferID, what string) int {
	if c.ended[id] {
		delete(c.ended, id)
		return 0
	}
	c.nextNum++
	c.offers[c.nextNum], c.numOf[id], c.about[id] = id, c.nextNum, what
	return c.nextNum
}

// Observe tracks transfer events so commands can refer to them by number.
// Call it for every engine event before rendering it.
func (c *Controller) Observe(ev core.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e := ev.(type) {
	case core.ImageOffered:
		c.track(e.ID, Offer(e)+" from "+c.Names.Nick(e.Peer))
	case core.TransferDone:
		if !e.Peer.Outgoing && e.Image {
			c.images = append(c.images, e.Path)
		}
		c.forget(e.ID)
	case core.TransferFailed:
		c.forget(e.ID)
	}
}

// forget drops an ended transfer. One without a number yet is remembered for
// a while: a send can fail before SendFile has returned its id. Caller holds mu.
func (c *Controller) forget(id core.TransferID) {
	delete(c.about, id)
	n, ok := c.numOf[id]
	if !ok {
		if len(c.ended) >= 256 {
			clear(c.ended)
		}
		c.ended[id] = true
		return
	}
	delete(c.offers, n)
	delete(c.numOf, id)
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

// Resolve finds the one contact that name stands for: a nickname, a whole
// fingerprint, or the first eight or more characters of one. When it could
// mean more than one contact the answer is an error, never a guess: a contact
// may have named itself after someone else's nickname or fingerprint.
func (c *Controller) Resolve(name string) (core.PeerID, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return core.PeerID{}, errors.New("no contact given")
	}
	var found []core.Contact
	prefix := strings.ToLower(strings.ReplaceAll(name, " ", ""))
	for _, ct := range c.E.Contacts() {
		if ct.Nickname == name || len(prefix) >= 8 && strings.HasPrefix(ct.ID.Fingerprint(), prefix) {
			found = append(found, ct)
		}
	}
	switch len(found) {
	case 0:
		return core.PeerID{}, core.ErrUnknownContact
	case 1:
		return found[0].ID, nil
	}
	return core.PeerID{}, fmt.Errorf("%q could mean %d contacts; give more of the fingerprint (see /contacts), or /rename one of them", name, len(found))
}

// firstArg splits off the first argument of a command: a quoted string, or
// the first word. Quotes are how a path or a name with spaces is given.
func firstArg(rest string) (arg, remainder string, err error) {
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end < 0 {
			return "", "", errors.New(`a quote is not closed: write "a name or path with spaces" between two quotes`)
		}
		return rest[1 : 1+end], strings.TrimSpace(rest[end+2:]), nil
	}
	arg, remainder, _ = strings.Cut(rest, " ")
	return arg, strings.TrimSpace(remainder), nil
}

// resolveFirst finds the contact at the start of rest and returns what
// follows it. Nicknames may contain spaces, so every way of cutting rest at
// a space is tried; when more than one way names a contact ("Bob" and "Bob
// Smith" both exist), the command is refused rather than sent to a guess.
func (c *Controller) resolveFirst(rest string) (core.PeerID, string, error) {
	if strings.HasPrefix(rest, `"`) {
		name, remainder, err := firstArg(rest)
		if err != nil {
			return core.PeerID{}, "", err
		}
		id, err := c.Resolve(name)
		return id, remainder, err
	}
	var id core.PeerID
	var remainder string
	var names []string
	var firstErr error
	for i := 0; i <= len(rest); i++ {
		if i < len(rest) && rest[i] != ' ' {
			continue
		}
		found, err := c.Resolve(rest[:i])
		switch {
		case err == nil && len(names) > 0 && found == id:
			// The same contact again: a fingerprint pasted in its groups of
			// four resolves at every group. The longest reading is the one.
			remainder = strings.TrimSpace(rest[i:])
		case err == nil:
			if len(names) > 0 {
				return core.PeerID{}, "", fmt.Errorf("%s and %q are different contacts; put the name in quotes, or use a fingerprint", strings.Join(names, " and "), rest[:i])
			}
			id, remainder = found, strings.TrimSpace(rest[i:])
			names = append(names, strconv.Quote(rest[:i]))
		case !errors.Is(err, core.ErrUnknownContact) && firstErr == nil:
			firstErr = err
		}
	}
	switch {
	case len(names) > 0:
		return id, remainder, nil
	case firstErr != nil:
		return core.PeerID{}, "", firstErr
	}
	return core.PeerID{}, "", core.ErrUnknownContact
}

// Help lists the commands.
const Help = `/connect <invite|contact>   connect (an invite string starts with burrow1:)
/to <contact>               select the conversation for plain text lines
/msg <contact> <text>       send to a specific contact ("a name with spaces" goes in quotes)
/contacts                   list contacts (* online; each line ends in verified or unverified)
/invite [multi] [tor] [qr] [host] create an invite (single-use, 1 h; tor = onion, qr = also as QR code)
/safety <contact>           show the safety number to compare out of band
/verify <contact>           mark verified after comparing safety numbers
/rename <contact> <name>    set a nickname
/block|/unblock <contact>   block or unblock
/remove <contact>           delete a contact
/disconnect <contact>       close the session
/image <path> [caption]     send an image (metadata is stripped first; "a path with spaces" goes in quotes)
/file <path> [caption]      send any file (.zip, .pdf, …) as it is; only its type is revealed, never its name.
                            Pictures lose their metadata. /file --as-is sends a photo or video format
                            Burrow cannot clean (HEIC, RAW, MP4…) with its location and camera data
/accept <n> | /reject <n>   answer an offer by its number; the offer shows the size first
/cancel <n>                 cancel a transfer you are sending or receiving
/transfers                  list offers and running transfers, with their sizes
/partials                   list partial downloads that can resume
/view <n>                   show a received image larger, in the chat (terminals with true colour), n from /images
/images                     list received images
/history [n]                show the last n stored messages (needs history = true)
/typing on|off              send typing indicators (needs typing = true in the settings)
/id                         show your fingerprint
/quit                       exit`

// Exec runs one input line against the conversation selected now. Lines
// without a leading slash are messages to that contact. It returns output
// lines and whether the UI should quit.
func (c *Controller) Exec(ctx context.Context, line string) (out []string, quit bool) {
	current, selected := c.Selected()
	return c.ExecFor(ctx, line, current, selected)
}

// ExecFor is Exec against the conversation that was selected when the user
// entered the line, which may not be the selected one by the time the line
// runs: a message goes to the contact the user was looking at.
func (c *Controller) ExecFor(ctx context.Context, line string, current core.PeerID, selected bool) (out []string, quit bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, false
	}
	errNone := errors.New("no conversation selected; use /to <contact>")
	fail := func(err error) ([]string, bool) { return []string{"! " + err.Error()}, false }
	if !strings.HasPrefix(line, "/") {
		switch {
		case !selected:
			return fail(errNone)
		case strings.HasPrefix(line, "burrow1:"):
			// An invite is a password: pasted into the chat by mistake it
			// would reach whoever the conversation is with.
			return fail(errors.New("that is an invite and it was not sent; /connect <invite> uses it"))
		}
		return c.send(current, line), false
	}
	cmd, rest, _ := strings.Cut(line[1:], " ")
	rest = strings.TrimSpace(rest)
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
		if !selected {
			return fail(errNone)
		}
		n, _ := strconv.Atoi(rest)
		hist, err := c.E.History(current, n)
		if err != nil {
			return fail(err)
		}
		for _, h := range hist {
			line := fmt.Sprintf("<%s> %s", c.Names.Named(h.Peer), Body(h.Text))
			if h.Mine {
				line = Mine(h.Text)
			}
			out = append(out, h.At.Local().Format("2006-01-02 15:04 ")+line)
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
			// Copied from a screen where it wrapped, an invite arrives with
			// spaces where its rows broke. It never contains one itself.
			target.Invite = strings.Join(strings.Fields(rest), "")
			if info, err := core.DescribeInvite(target.Invite); err == nil && info.Hostname {
				out = append(out, dnsNote(info.Host))
			}
		} else {
			id, err := c.Resolve(strings.Trim(rest, `"`))
			if err != nil {
				return fail(err)
			}
			target.Contact = &id
		}
		return append(out, c.connect(ctx, target)), false
	case "to":
		id, err := c.Resolve(strings.Trim(rest, `"`))
		if err != nil {
			return fail(err)
		}
		c.Select(id)
		return []string{"* now talking to " + c.Names.Label(id)}, false
	case "msg":
		id, text, err := c.resolveFirst(rest)
		if err != nil {
			return fail(err)
		}
		return c.send(id, text), false
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
			v := Verification(ct.Verified)
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
				// A host is an IP address or a name with a dot in it; anything
				// else is a mistyped option, not where contacts should dial.
				if _, _, err := net.SplitHostPort(f); err == nil && net.ParseIP(f) == nil {
					return fail(fmt.Errorf("give the address without a port (%q): the port is your listen_port", f))
				}
				if net.ParseIP(f) == nil && !strings.Contains(f, ".") {
					return fail(fmt.Errorf("unknown /invite option %q (multi, tor, qr, or the address others can reach you at)", f))
				}
				opts.Host = f
			}
		}
		if strings.HasSuffix(opts.Host, ".onion") {
			opts.Kind = "tor" // an onion address is only reachable through Tor
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
		id, err := c.Resolve(strings.Trim(rest, `"`))
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
		id, err := c.Resolve(strings.Trim(rest, `"`))
		if err != nil {
			return fail(err)
		}
		if err := c.E.VerifyContact(id); err != nil {
			return fail(err)
		}
		return []string{"* " + c.Names.Label(id) + " marked verified"}, false
	case "rename":
		id, name, err := c.resolveFirst(rest)
		if err != nil {
			return fail(err)
		}
		if err := c.E.RenameContact(id, strings.Trim(name, `"`)); err != nil {
			return fail(err)
		}
		return []string{"* renamed to " + c.Names.Label(id)}, false
	case "block", "unblock":
		id, err := c.Resolve(strings.Trim(rest, `"`))
		if err != nil {
			return fail(err)
		}
		if err := c.E.BlockContact(id, cmd == "block"); err != nil {
			return fail(err)
		}
		return []string{"* " + cmd + "ed " + c.Names.Label(id)}, false
	case "remove":
		id, err := c.Resolve(strings.Trim(rest, `"`))
		if err != nil {
			return fail(err)
		}
		label := c.Names.Label(id)
		if err := c.E.RemoveContact(id); err != nil {
			return fail(err)
		}
		if selected && current == id {
			c.Deselect()
		}
		return []string{"* removed " + label}, false
	case "disconnect":
		id, err := c.Resolve(strings.Trim(rest, `"`))
		if err != nil {
			return fail(err)
		}
		if err := c.E.Disconnect(id); err != nil {
			return fail(err)
		}
		return []string{"* disconnecting from " + c.Names.Label(id)}, false
	case "image", "file":
		if !selected {
			return fail(errNone)
		}
		asIs := false
		if after, ok := strings.CutPrefix(rest, "--as-is "); ok && cmd == "file" {
			asIs, rest = true, strings.TrimSpace(after)
		}
		path, caption, err := firstArg(rest)
		if err != nil {
			return fail(err)
		}
		if path == "" {
			return fail(fmt.Errorf(`usage: /%s <path> [caption]   (a path with spaces: /%s "my file.zip")`, cmd, cmd))
		}
		var id core.TransferID
		note := "metadata is stripped first"
		if cmd == "file" {
			note = "pictures lose their metadata; other files are sent as they are"
			id, err = c.E.SendFile(ctx, current, path, caption, asIs)
		} else {
			id, err = c.E.SendImage(ctx, current, path, caption)
		}
		if errors.Is(err, core.ErrKeepsMetadata) {
			return fail(fmt.Errorf("%w; convert it to JPEG or PNG first, or send it as it is with /file --as-is <path>", err))
		}
		if err != nil {
			return fail(err)
		}
		c.mu.Lock()
		n := c.track(id, cmd+" "+filepath.Base(path)+" to "+c.Names.Nick(current))
		c.mu.Unlock()
		if n == 0 {
			return nil, false // it has ended already; the event says how
		}
		return []string{fmt.Sprintf("* preparing %s #%d (%s); /cancel %d stops it", cmd, n, note, n)}, false
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
		sort.Ints(nums)
		for _, n := range nums {
			out = append(out, fmt.Sprintf("#%d %s", n, c.about[c.offers[n]]))
		}
		c.mu.Unlock()
		if len(out) == 0 {
			return []string{"(no transfers)"}, false
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
		if rest != "on" && rest != "off" {
			return fail(errors.New("usage: /typing on|off"))
		}
		if rest == "on" && !c.E.TypingEnabled() {
			return fail(errors.New("typing indicators are turned off in the settings; run `burrow config set typing true` and start again"))
		}
		c.mu.Lock()
		c.typing = rest == "on"
		c.mu.Unlock()
		return []string{"* typing indicators " + rest}, false
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

// Line renders an event like Names.Line, with the short number of an offer
// filled in, so that the line says exactly what to type.
func (c *Controller) Line(ev core.Event) string {
	line := c.Names.Line(ev)
	if o, ok := ev.(core.ImageOffered); ok {
		if n := c.Number(o.ID); n > 0 {
			line = strings.Replace(line, "/accept <n> or /reject <n>", fmt.Sprintf("/accept %d or /reject %d", n, n), 1)
		}
	}
	return line
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
	current, selected := c.Selected()
	if !selected {
		return s
	}
	ct, err := c.E.Contact(current)
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
		out = append(out, dnsNote(info.Host))
	}
	return append(out, c.connect(ctx, core.Target{InviteBytes: inv}))
}

func dnsNote(host string) string {
	return "note: this invite uses the hostname " + host + "; looking it up tells your DNS resolver which host you are contacting"
}

// connect dials target and selects the conversation, unless the user has
// selected another one while the connection was being made.
func (c *Controller) connect(ctx context.Context, target core.Target) string {
	since := c.selections()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	id, err := c.E.Connect(cctx, target)
	if err != nil {
		return "! " + err.Error()
	}
	if c.selectAfter(since, id) {
		return "* connected to " + c.Names.Label(id) + "; now talking to them"
	}
	return "* connected to " + c.Names.Label(id)
}
