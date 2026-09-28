// Package transport defines how Burrow reaches a peer. The handshake and
// session layers see only net.Conn. Every implementation passes
// internal/transport/conformance.
package transport

import (
	"context"
	"net"
)

// Kind names a transport.
type Kind string

// Kinds.
const (
	KindTCP Kind = "tcp"
	KindTor Kind = "tor"
)

// Address is a peer endpoint as carried by invites and contacts.
type Address struct {
	Kind Kind
	Host string // IP literal, hostname, or .onion
	Port uint16
}

// Listener accepts inbound connections.
type Listener interface {
	Accept() (net.Conn, error)
	Close() error
	Addr() net.Addr
}

// Transport listens and dials.
type Transport interface {
	Listen(ctx context.Context) (Listener, error)
	Dial(ctx context.Context, addr Address) (net.Conn, error)
	Kind() Kind
	// RateKey maps a remote address to the key used by the handshake failure
	// limiter (§3.4): a /24 or /48 for TCP, "" for transports without a source address.
	RateKey(remote net.Addr) string
}
