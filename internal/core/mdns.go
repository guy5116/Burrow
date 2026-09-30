package core

import (
	"crypto/rand"
	"net"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/mdns"
)

// runMDNS announces this instance (random name, per-session nonce, keyed tag)
// and dials a contact at most once per announcement nonce when its tag
// matches. Strangers learn only that a Burrow instance is on the LAN.
func (e *Engine) runMDNS(port uint16) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return
	}
	tag := identity.MDNSTag(nonce, e.id.Public())
	ann, err := mdns.NewAnnouncer(port, nonce, tag)
	if err != nil {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := ann.Run(e.ctx); err != nil {
			e.log.Warn("mdns announce", "err", err.Error())
		}
	}()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		if err := mdns.Browse(e.ctx, e.onAnnouncement); err != nil {
			e.log.Warn("mdns browse", "err", err.Error())
		}
	}()
}

// onAnnouncement matches the tag against every contact and dials once.
func (e *Engine) onAnnouncement(an mdns.Announcement) {
	if id, ok := e.matchAnnouncement(an.Nonce, an.Tag); ok {
		host := an.IP.String()
		if an.Zone != "" { // a link-local IPv6 address is only reachable through its interface
			host += "%" + an.Zone
		}
		addr := transport.Address{Kind: transport.KindTCP, Host: host, Port: an.Port}
		// The address is used, not saved: anyone on the LAN can send an
		// announcement, and a saved address is where reconnects go. Failures
		// are not shown either: nobody asked for this dial.
		e.spawn(nil, func() { _ = e.dial(e.ctx, addr, id, dialOpts{quiet: true, unsaved: true}) })
	}
}

// matchAnnouncement returns the contact whose key produces tag for nonce, if
// any, and records the nonce so the same announcement is not dialed twice.
func (e *Engine) matchAnnouncement(nonce, tag [16]byte) (id PeerID, ok bool) {
	e.do(func() {
		if e.mdnsSeen[nonce] {
			return
		}
		// Every nonce is examined once, a stranger's too: repeating an
		// announcement costs a map lookup, not a hash per contact.
		if len(e.mdnsSeen) >= mdnsSeenMax {
			clear(e.mdnsSeen)
		}
		e.mdnsSeen[nonce] = true
		for cid, c := range e.contacts {
			if !c.Blocked && e.peers[cid] == nil && identity.MDNSTag(nonce, cid) == tag {
				id, ok = cid, true
				return
			}
		}
	})
	return id, ok
}

const mdnsSeenMax = 4096

// listenPortOf finds the TCP listener's port for announcements.
func (e *Engine) listenPortOf() uint16 {
	for _, a := range e.ListenAddrs() {
		if ta, ok := a.(*net.TCPAddr); ok {
			return uint16(ta.Port) // #nosec G115 -- port range
		}
	}
	return e.cfg.listenPort()
}
