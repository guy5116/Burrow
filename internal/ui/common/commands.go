package common

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	Typing     bool
}

// NewController wires a controller to an engine.
func NewController(e *core.Engine, inviteHost string) *Controller {
	return &Controller{E: e, Names: Names{E: e}, InviteHost: inviteHost}
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
/invite [multi] [host]      create an invite (default single-use, 1 h)
/safety <contact>           show the safety number to compare out of band
/verify <contact>           mark verified after comparing safety numbers
/rename <contact> <name>    set a nickname
/block|/unblock <contact>   block or unblock
/remove <contact>           delete a contact
/disconnect <contact>       close the session
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
		return []string{"your fingerprint: " + id.Display}, false
	case "connect":
		if rest == "" {
			return fail(errors.New("usage: /connect <invite|contact>"))
		}
		var target core.Target
		if strings.HasPrefix(rest, "burrow1:") {
			target.Invite = rest
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
			return fail(err)
		}
		c.Current = &id
		return []string{"* connected to " + c.Names.Label(id) + "; now talking to them"}, false
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
		for _, f := range strings.Fields(rest) {
			switch f {
			case "multi", "--multi-use":
				opts.MultiUse = true
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
		return []string{fmt.Sprintf("invite (%s, expires %s) — share it over a channel you trust, treat it like a password:", kind, inv.Expiry.Local().Format(time.RFC822)), inv.String}, false
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
