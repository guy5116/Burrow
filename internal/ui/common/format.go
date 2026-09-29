// Package common holds formatting and command handling shared by both UIs.
// It never sanitizes: every string it sees already passed internal/text in core.
package common

import (
	"fmt"
	"strings"
	"time"

	"github.com/guy5116/burrow/internal/core"
)

// Names resolves peer ids to display labels.
type Names struct{ E *core.Engine }

// Label returns "nick (fp8)" or the short fingerprint for an unknown peer.
func (n Names) Label(id core.PeerID) string {
	c, err := n.E.Contact(id)
	if err != nil {
		return id.Short()
	}
	mark := ""
	if c.Verified {
		mark = "✓"
	}
	if c.Nickname == "" || c.Nickname == id.Short() {
		return id.Short() + mark // an unnamed contact: do not print the same code twice
	}
	return fmt.Sprintf("%s%s (%s)", c.Nickname, mark, id.Short())
}

// Nick returns just the nickname (or short fingerprint).
func (n Names) Nick(id core.PeerID) string {
	if c, err := n.E.Contact(id); err == nil && c.Nickname != "" {
		return c.Nickname
	}
	return id.Short()
}

// Line renders an event as one human-readable line; "" means nothing to show.
func (n Names) Line(ev core.Event) string {
	switch e := ev.(type) {
	case core.PeerConnected:
		return fmt.Sprintf("* %s connected (%s)", n.Label(e.Peer), e.Transport)
	case core.PeerDisconnected:
		return fmt.Sprintf("* %s disconnected: %s", n.Label(e.Peer), e.Reason)
	case core.Reconnecting:
		return fmt.Sprintf("* reconnecting to %s (attempt %d) at %s", n.Label(e.Peer), e.Attempt, e.NextAt.Format("15:04:05"))
	case core.HandshakeFailed:
		who := "peer"
		if e.Peer != (core.PeerID{}) {
			who = n.Label(e.Peer)
		}
		return fmt.Sprintf("! could not establish a secure session with %s: %s", who, e.Reason)
	case core.NewPeerViaInvite:
		how := "added from the invite you used"
		if e.InviteID != "" {
			how = "joined with your invite " + e.InviteID
		}
		return fmt.Sprintf("* new contact %s %s — UNVERIFIED until you compare safety numbers (/safety)", n.Label(e.Peer), how)
	case core.InviteConsumed:
		return fmt.Sprintf("* invite %s used", e.ID)
	case core.NameCollision:
		return fmt.Sprintf("! NAME COLLISION: new contact %s uses the name %q like existing contact %s — check fingerprints", n.Label(e.New), e.Name, n.Label(e.Existing))
	case core.PeerVerified:
		return fmt.Sprintf("* %s marked verified", n.Label(e.Peer))
	case core.MessageReceived:
		ts := ""
		if !e.SentAt.IsZero() {
			ts = e.SentAt.Local().Format("15:04 ")
		}
		return fmt.Sprintf("%s<%s> %s", ts, n.Nick(e.Peer), e.Text)
	case core.MessageStatus:
		return fmt.Sprintf("* message %016x to %s: %s", uint64(e.ID), n.Nick(e.Peer), e.Status)
	case core.Typing:
		if e.Typing {
			return fmt.Sprintf("* %s is typing…", n.Nick(e.Peer))
		}
		return ""
	case core.ImageOffered:
		return fmt.Sprintf("* %s offers %s%s — /accept <n> or /reject <n> (see /transfers)", n.Nick(e.Peer), Offer(e), captionSuffix(e.Caption))
	case core.TransferProgress:
		return "" // too chatty for text mode; JSON carries it
	case core.TransferResumed:
		return "* resuming a partial download"
	case core.TransferDone:
		if e.Peer.Outgoing {
			return fmt.Sprintf("* %s delivered to %s", Kind(e.Image), n.Nick(e.Peer.Peer))
		}
		return fmt.Sprintf("* %s from %s saved: %s", Kind(e.Image), n.Nick(e.Peer.Peer), e.Path)
	case core.TransferFailed:
		dir := "to"
		if !e.Peer.Outgoing {
			dir = "from"
		}
		return fmt.Sprintf("! transfer %s %s failed: %s", dir, n.Nick(e.Peer.Peer), e.Reason)
	case core.ErrorEvent:
		return "! " + e.Message
	}
	return ""
}

func captionSuffix(c string) string {
	if c == "" {
		return ""
	}
	return fmt.Sprintf(" %q", c)
}

// Size renders bytes as a short human unit.
func Size(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// IPWarning is shown when an identity is created, by the CLI and the desktop
// app alike: encryption hides the content, not the addresses.
const IPWarning = `WARNING: Burrow hides what you send, not where you are.
  - The person you talk to sees your IP address.
  - Anyone watching the network between you, such as someone on the same Wi-Fi
    or your internet provider, sees both IP addresses. They cannot read your
    messages or files.
  To hide your IP address, install tor and run: burrow config set transport tor`

// Kind is "image" or "file".
func Kind(image bool) string {
	if image {
		return "image"
	}
	return "file"
}

// Offer describes what a peer offers, size first: it is what the user
// decides on before anything is downloaded.
func Offer(e core.ImageOffered) string {
	if e.Format != core.FormatFile {
		return fmt.Sprintf("an image: %s, %s %d×%d", Size(e.Size), FormatName(e.Format), e.Width, e.Height)
	}
	if e.Ext == "" {
		return fmt.Sprintf("a file: %s, no file type given", Size(e.Size))
	}
	return fmt.Sprintf("a file: %s, type .%s", Size(e.Size), e.Ext)
}

// FormatName names a wire image format.
func FormatName(f uint8) string {
	switch f {
	case core.FormatFile:
		return "file"
	case 1:
		return "PNG"
	case 2:
		return "JPEG"
	case 3:
		return "WebP"
	case 4:
		return "GIF"
	}
	return "image"
}

func hexID(id core.TransferID) string { return fmt.Sprintf("%x", id[:]) }

// JSON renders an event as a flat map for --json output.
func (n Names) JSON(ev core.Event) map[string]any {
	m := map[string]any{"event": strings.TrimPrefix(fmt.Sprintf("%T", ev), "core.")}
	peer := func(id core.PeerID) {
		m["peer"] = id.Fingerprint()
		m["nick"] = n.Nick(id)
	}
	switch e := ev.(type) {
	case core.PeerConnected:
		peer(e.Peer)
		m["initiator"], m["transport"], m["name"] = e.Initiator, string(e.Transport), e.Name
	case core.PeerDisconnected:
		peer(e.Peer)
		m["reason"] = e.Reason
	case core.Reconnecting:
		peer(e.Peer)
		m["attempt"], m["next_at"] = e.Attempt, e.NextAt.Format(time.RFC3339)
	case core.HandshakeFailed:
		if e.Peer != (core.PeerID{}) {
			peer(e.Peer)
		}
		m["stage"], m["reason"] = e.Stage, e.Reason
	case core.NewPeerViaInvite:
		peer(e.Peer)
		m["invite_id"], m["own_invite"] = e.InviteID, e.InviteID != ""
	case core.InviteConsumed:
		m["invite_id"] = e.ID
	case core.NameCollision:
		m["new"], m["existing"], m["name"] = e.New.Fingerprint(), e.Existing.Fingerprint(), e.Name
	case core.PeerVerified:
		peer(e.Peer)
	case core.MessageReceived:
		peer(e.Peer)
		m["id"], m["text"] = fmt.Sprintf("%016x", uint64(e.ID)), e.Text
		if !e.SentAt.IsZero() {
			m["sent_at"] = e.SentAt.Unix()
		}
	case core.MessageStatus:
		peer(e.Peer)
		m["id"], m["status"] = fmt.Sprintf("%016x", uint64(e.ID)), e.Status.String()
	case core.Typing:
		peer(e.Peer)
		m["typing"] = e.Typing
	case core.ImageOffered:
		peer(e.Peer)
		m["id"], m["size"], m["format"], m["width"], m["height"], m["caption"] = hexID(e.ID), e.Size, FormatName(e.Format), e.Width, e.Height, e.Caption
		if e.Format == core.FormatFile {
			m["ext"] = e.Ext
		}
	case core.TransferProgress:
		peer(e.Peer.Peer)
		m["id"], m["outgoing"], m["done"], m["size"] = hexID(e.ID), e.Peer.Outgoing, e.Done, e.Size
	case core.TransferResumed:
		m["id"] = hexID(e.ID)
	case core.TransferDone:
		peer(e.Peer.Peer)
		m["id"], m["outgoing"], m["path"], m["image"] = hexID(e.ID), e.Peer.Outgoing, e.Path, e.Image
	case core.TransferFailed:
		peer(e.Peer.Peer)
		m["id"], m["outgoing"], m["reason"] = hexID(e.ID), e.Peer.Outgoing, e.Reason
	case core.ErrorEvent:
		m["message"] = e.Message
	}
	return m
}

// FormatSafety renders a safety number as three rows of four groups.
func FormatSafety(sn core.SafetyNumber) string {
	s := sn.String()
	g := strings.Split(s, " ")
	return strings.Join(g[0:4], " ") + "\n" + strings.Join(g[4:8], " ") + "\n" + strings.Join(g[8:12], " ")
}
