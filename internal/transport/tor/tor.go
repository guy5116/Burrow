// Package tor is the onion-service transport (CLAUDE.md §5, Phase 4). It
// controls a system `tor` binary through github.com/cretz/bine; Tor is never
// bundled. The onion service's ed25519 key is a second long-term secret that
// lives in the encrypted identity blob and reaches tor only through the
// control port (ADD_ONION), never through tor's data directory.
package tor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/cretz/bine/tor"

	"github.com/guy5116/burrow/internal/transport"
)

// Transport runs one tor process on demand.
type Transport struct {
	Key     ed25519.PrivateKey // onion service key (from the identity blob)
	Port    uint16             // virtual port announced in invites
	ExePath string             // "" → "tor" on PATH
	// Bootstrap bounds how long Listen waits for tor to build circuits.
	Bootstrap time.Duration

	mu     sync.Mutex
	proc   *tor.Tor
	dialer *tor.Dialer
	onion  *tor.OnionService
}

// ErrNoTor is returned when no tor binary can be found.
var ErrNoTor = errors.New("tor: no tor binary found (install tor or set tor_exe)")

// New builds a transport for the given onion key and virtual port.
func New(key ed25519.PrivateKey, port uint16, exePath string) *Transport {
	return &Transport{Key: key, Port: port, ExePath: exePath, Bootstrap: 3 * time.Minute}
}

// Kind returns "tor".
func (t *Transport) Kind() transport.Kind { return transport.KindTor }

// RateKey is empty: onion connections carry no source address (§3.4 uses a global limit).
func (t *Transport) RateKey(net.Addr) string { return "" }

// Available reports whether a tor binary is present.
func (t *Transport) Available() bool {
	exe := t.ExePath
	if exe == "" {
		exe = "tor"
	}
	_, err := exec.LookPath(exe)
	return err == nil
}

// start launches tor once (temporary data dir, deleted on Close) and waits for bootstrap.
func (t *Transport) start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc != nil {
		return nil
	}
	if !t.Available() {
		return ErrNoTor
	}
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	proc, err := tor.Start(bctx, &tor.StartConf{ExePath: t.ExePath, ProcessCreator: nil})
	if err != nil {
		return err
	}
	proc.DeleteDataDirOnClose = true
	proc.StopProcessOnClose = true
	d, err := proc.Dialer(bctx, nil)
	if err != nil {
		_ = proc.Close()
		return err
	}
	t.proc, t.dialer = proc, d
	return nil
}

// Listen publishes the onion service and returns its listener. The onion
// address is available afterwards through OnionAddress.
func (t *Transport) Listen(ctx context.Context) (transport.Listener, error) {
	if err := t.start(ctx); err != nil {
		return nil, err
	}
	local, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	svc, err := t.proc.Listen(bctx, &tor.ListenConf{Key: t.Key, RemotePorts: []int{int(t.Port)}, LocalListener: local, Version3: true})
	if err != nil {
		_ = local.Close()
		return nil, err
	}
	svc.CloseLocalListenerOnClose = true
	t.mu.Lock()
	t.onion = svc
	t.mu.Unlock()
	return svc, nil
}

// OnionAddress returns "<56 chars>.onion" once Listen succeeded ("" before).
func (t *Transport) OnionAddress() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onion == nil {
		return ""
	}
	return t.onion.ID + ".onion"
}

// Dial connects to an onion address through tor's SOCKS port.
func (t *Transport) Dial(ctx context.Context, addr transport.Address) (net.Conn, error) {
	if addr.Kind != transport.KindTor || addr.Host == "" || addr.Port == 0 {
		return nil, errors.New("tor: not an onion address")
	}
	if err := t.start(ctx); err != nil {
		return nil, err
	}
	return t.dialer.DialContext(ctx, "tcp", net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port))))
}

// Close stops tor and removes its temporary data directory.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.onion != nil {
		_ = t.onion.Close()
		t.onion = nil
	}
	if t.proc != nil {
		err := t.proc.Close()
		t.proc = nil
		return err
	}
	return nil
}
