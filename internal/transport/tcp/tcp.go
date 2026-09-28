// Package tcp is the direct TCP transport (CLAUDE.md §5, Phase 1).
package tcp

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"

	"github.com/guy5116/burrow/internal/transport"
)

// Transport listens on ListenAddr (e.g. ":47337", dual-stack) and dials peers.
type Transport struct {
	ListenAddr  string
	DialTimeout time.Duration
}

// New returns a TCP transport listening on the given address.
func New(listenAddr string) *Transport {
	return &Transport{ListenAddr: listenAddr, DialTimeout: 15 * time.Second}
}

// Kind returns "tcp".
func (t *Transport) Kind() transport.Kind { return transport.KindTCP }

var keepAlive = net.KeepAliveConfig{Enable: true, Idle: 30 * time.Second, Interval: 10 * time.Second, Count: 3}

// Listen opens the listening socket. Accepted connections have TCP_NODELAY
// (Go's default) and OS keepalives on.
func (t *Transport) Listen(ctx context.Context) (transport.Listener, error) {
	lc := net.ListenConfig{KeepAliveConfig: keepAlive}
	return lc.Listen(ctx, "tcp", t.ListenAddr)
}

// ErrBadAddress is returned for an address that is not this transport's kind.
var ErrBadAddress = errors.New("tcp: address is not a tcp address")

// Dial connects to addr. Hostnames are resolved by the OS resolver; the UI
// warns about that before the user chooses a hostname.
func (t *Transport) Dial(ctx context.Context, addr transport.Address) (net.Conn, error) {
	if addr.Kind != transport.KindTCP || addr.Host == "" || addr.Port == 0 {
		return nil, ErrBadAddress
	}
	d := net.Dialer{Timeout: t.DialTimeout, KeepAliveConfig: keepAlive}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port))))
}

// RateKey returns the /24 (IPv4) or /48 (IPv6) prefix of remote.
func (t *Transport) RateKey(remote net.Addr) string {
	if remote == nil {
		return ""
	}
	ap, err := netip.ParseAddrPort(remote.String())
	if err != nil {
		return remote.String()
	}
	a := ap.Addr().Unmap()
	bits := 48
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return a.String()
	}
	return p.String()
}
