// Package mdns is opt-in LAN discovery (CLAUDE.md §5). An instance announces a
// random per-session service name, its port, and a TXT record carrying
// nonce[16] || tag[16] where tag = identity.MDNSTag(nonce, pubkey). Anyone
// else on the LAN learns only that some Burrow instance exists. Nothing here
// probes or handshakes; core recognizes contacts by recomputing the tag.
// Packets are hand-built with golang.org/x/net/dns/dnsmessage.
package mdns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

const (
	service  = "_burrow._tcp.local."
	group    = "224.0.0.251:5353"
	group6   = "[ff02::fb]:5353"
	interval = 30 * time.Second
	ttl      = 60
)

// families are the multicast groups used, IPv4 first. IPv6 is best effort:
// a host without IPv6 multicast still works over IPv4.
var families = []struct{ network, group string }{{"udp4", group}, {"udp6", group6}}

// Announcement is what a browser learns from one packet.
type Announcement struct {
	Name  string
	IP    net.IP // the packet's source address, never a claimed A record
	Zone  string // the interface of a link-local IPv6 source, needed to dial it
	Port  uint16
	Nonce [16]byte
	Tag   [16]byte
}

// Announcer multicasts unsolicited announcements every 30 s.
type Announcer struct {
	name  string
	port  uint16
	nonce [16]byte
	tag   [16]byte
}

// NewAnnouncer picks a random service name for this session.
func NewAnnouncer(port uint16, nonce, tag [16]byte) (*Announcer, error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	return &Announcer{name: hex.EncodeToString(r[:]), port: port, nonce: nonce, tag: tag}, nil
}

// Packet builds the announcement (a response with PTR, SRV and TXT records).
func (a *Announcer) Packet() ([]byte, error) {
	inst := a.name + "." + service
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	svc, err := dnsmessage.NewName(service)
	if err != nil {
		return nil, err
	}
	in, err := dnsmessage.NewName(inst)
	if err != nil {
		return nil, err
	}
	host, err := dnsmessage.NewName(a.name + ".local.")
	if err != nil {
		return nil, err
	}
	if err := b.PTRResource(dnsmessage.ResourceHeader{Name: svc, Type: dnsmessage.TypePTR, Class: dnsmessage.ClassINET, TTL: ttl}, dnsmessage.PTRResource{PTR: in}); err != nil {
		return nil, err
	}
	if err := b.SRVResource(dnsmessage.ResourceHeader{Name: in, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET, TTL: ttl}, dnsmessage.SRVResource{Port: a.port, Target: host}); err != nil {
		return nil, err
	}
	txt := dnsmessage.TXTResource{TXT: []string{"n=" + hex.EncodeToString(a.nonce[:]), "t=" + hex.EncodeToString(a.tag[:])}}
	if err := b.TXTResource(dnsmessage.ResourceHeader{Name: in, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: ttl}, txt); err != nil {
		return nil, err
	}
	return b.Finish()
}

// Run announces on every available family until ctx ends. It fails only when
// no family can be used.
func (a *Announcer) Run(ctx context.Context) error {
	pkt, err := a.Packet()
	if err != nil {
		return err
	}
	type out struct {
		conn *net.UDPConn
		dst  *net.UDPAddr
	}
	var outs []out
	var firstErr error
	for _, f := range families {
		dst, err := net.ResolveUDPAddr(f.network, f.group)
		if err == nil {
			var conn *net.UDPConn
			if conn, err = net.ListenUDP(f.network, nil); err == nil {
				outs = append(outs, out{conn, dst})
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if len(outs) == 0 {
		return firstErr
	}
	defer func() {
		for _, o := range outs {
			_ = o.conn.Close()
		}
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		for _, o := range outs {
			_, _ = o.conn.WriteToUDP(pkt, o.dst)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Parse extracts an Announcement from a packet; ok is false for anything
// that is not a well-formed Burrow announcement. It never panics.
func Parse(pkt []byte, src net.IP) (Announcement, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(pkt)
	if err != nil || !h.Response {
		return Announcement{}, false
	}
	if err := p.SkipAllQuestions(); err != nil {
		return Announcement{}, false
	}
	var an Announcement
	var haveSRV, haveTXT bool
	for i := 0; i < 32; i++ {
		rh, err := p.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return Announcement{}, false
		}
		name := rh.Name.String()
		if len(name) <= len(service) || name[len(name)-len(service):] != service {
			if err := p.SkipAnswer(); err != nil {
				return Announcement{}, false
			}
			continue
		}
		switch rh.Type {
		case dnsmessage.TypeSRV:
			r, err := p.SRVResource()
			if err != nil {
				return Announcement{}, false
			}
			an.Port, an.Name, haveSRV = r.Port, name[:len(name)-len(service)-1], true
		case dnsmessage.TypeTXT:
			r, err := p.TXTResource()
			if err != nil {
				return Announcement{}, false
			}
			var n, t bool
			for _, s := range r.TXT {
				switch {
				case len(s) == 34 && s[:2] == "n=":
					n = decode16(s[2:], &an.Nonce)
				case len(s) == 34 && s[:2] == "t=":
					t = decode16(s[2:], &an.Tag)
				}
			}
			haveTXT = n && t
		default:
			if err := p.SkipAnswer(); err != nil {
				return Announcement{}, false
			}
		}
	}
	if !haveSRV || !haveTXT || an.Port == 0 {
		return Announcement{}, false
	}
	an.IP = src
	return an, true
}

func decode16(s string, dst *[16]byte) bool {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return false
	}
	copy(dst[:], b)
	return true
}

// browseBudget is how many announcements per second Browse passes on. The
// rest are dropped: a host that floods the LAN must not keep the engine busy.
const browseBudget = 20

// budget is a counter that refills once a second.
type budget struct {
	mu     sync.Mutex
	second int64
	used   int
}

func (b *budget) take(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := now.Unix(); s != b.second {
		b.second, b.used = s, 0
	}
	b.used++
	return b.used <= browseBudget
}

// Browse delivers announcements from other instances on every available
// family until ctx ends, one call of fn at a time. It fails only when no
// family can be joined.
func Browse(ctx context.Context, fn func(Announcement)) error {
	var conns []*net.UDPConn
	var firstErr error
	for _, f := range families {
		addr, err := net.ResolveUDPAddr(f.network, f.group)
		if err == nil {
			var conn *net.UDPConn
			if conn, err = net.ListenMulticastUDP(f.network, nil, addr); err == nil {
				conns = append(conns, conn)
				continue
			}
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if len(conns) == 0 {
		return firstErr
	}
	stop := context.AfterFunc(ctx, func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
	defer stop()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var limit budget
	for _, conn := range conns {
		wg.Add(1)
		go func(conn *net.UDPConn) {
			defer wg.Done()
			defer func() { _ = conn.Close() }()
			buf := make([]byte, 9000)
			for {
				n, src, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				if an, ok := Parse(buf[:n], src.IP); ok && limit.take(time.Now()) {
					an.Zone = src.Zone
					mu.Lock()
					fn(an)
					mu.Unlock()
				}
			}
		}(conn)
	}
	wg.Wait()
	return nil
}
