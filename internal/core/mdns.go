package core

import (
	"context"
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
		addr := transport.Address{Kind: transport.KindTCP, Host: an.IP.String(), Port: an.Port}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			ctx, cancel := context.WithCancel(e.ctx)
			defer cancel()
			_ = e.dial(ctx, addr, id, nil, "") // dial records the address on success
		}()
	}
}

// matchAnnouncement returns the contact whose key produces tag for nonce, if
// any, and records the nonce so the same announcement is not dialed twice.
func (e *Engine) matchAnnouncement(nonce, tag [16]byte) (id PeerID, ok bool) {
	e.do(func() {
		if e.mdnsSeen == nil {
			e.mdnsSeen = map[[16]byte]bool{}
		}
		if e.mdnsSeen[nonce] {
			return
		}
		for cid, c := range e.contacts {
			if c.Blocked || e.peers[cid] != nil {
				continue
			}
			if identity.MDNSTag(nonce, cid) == tag {
				e.mdnsSeen[nonce] = true
				if len(e.mdnsSeen) > 4096 {
					e.mdnsSeen = map[[16]byte]bool{nonce: true}
				}
				id, ok = cid, true
				return
			}
		}
	})
	return id, ok
}

// listenPortOf finds the TCP listener's port for announcements.
func (e *Engine) listenPortOf() uint16 {
	for _, a := range e.ListenAddrs() {
		if ta, ok := a.(*net.TCPAddr); ok {
			return uint16(ta.Port) // #nosec G115 -- port range
		}
	}
	return e.cfg.listenPort()
}
