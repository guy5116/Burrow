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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cretz/bine/process"
	"github.com/cretz/bine/tor"

	"github.com/guy5116/burrow/internal/transport"
)

// Transport runs one tor process on demand.
type Transport struct {
	Key     ed25519.PrivateKey // onion service key (from the identity blob); wiped by Close
	Port    uint16             // virtual port announced in invites
	ExePath string             // "" → "tor" on PATH
	// DataDir is where tor keeps its state while it runs: a private directory
	// of ours, emptied at every start. "" → the system's temporary directory.
	DataDir string
	// Bootstrap bounds how long starting tor and publishing the service may take.
	Bootstrap time.Duration

	mu     sync.Mutex
	proc   *tor.Tor
	kill   context.CancelFunc // ends the tor process
	dialer *tor.Dialer
	onion  *tor.OnionService
}

// ErrNoTor is returned when no tor binary can be found.
var ErrNoTor = errors.New("tor: no tor binary found (install tor or set tor_exe)")

// New builds a transport for the given onion key and virtual port.
func New(key ed25519.PrivateKey, port uint16, exePath, dataDir string) *Transport {
	return &Transport{Key: key, Port: port, ExePath: exePath, DataDir: dataDir, Bootstrap: 3 * time.Minute}
}

// Kind returns "tor".
func (t *Transport) Kind() transport.Kind { return transport.KindTor }

// RateKey is empty: onion connections carry no source address (§3.4 uses a global limit).
func (t *Transport) RateKey(net.Addr) string { return "" }

// exe is the tor binary to run.
func (t *Transport) exe() string {
	if t.ExePath == "" {
		return "tor"
	}
	return t.ExePath
}

// Available reports whether a tor binary is present.
func (t *Transport) Available() bool {
	_, err := exec.LookPath(t.exe())
	return err == nil
}

// startTor is tor.Start; tests replace it.
var startTor = tor.Start

// launch starts tor with a lifetime of its own. ctx bounds the start only:
// when it ends before tor is up, tor is killed; once tor is up, ending ctx
// no longer touches it, and kill is the way to stop it.
func launch(ctx context.Context, conf *tor.StartConf) (proc *tor.Tor, kill context.CancelFunc, err error) {
	life, kill := context.WithCancel(context.Background())
	detach := context.AfterFunc(ctx, kill)
	proc, err = startTor(life, conf)
	if !detach() && err == nil { // ctx ended while tor was starting
		_ = proc.Close()
		err = ctx.Err()
	}
	if err != nil {
		kill()
		return nil, nil, err
	}
	return proc, kill, nil
}

// start launches tor once and waits for bootstrap. tor's state lives in a
// fresh directory under DataDir that is deleted on Close; what a crash left
// there is deleted first. tor's own output is discarded: it must reach
// neither the chat nor the JSON event stream.
func (t *Transport) start(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.proc != nil {
		return nil
	}
	if !t.Available() {
		return ErrNoTor
	}
	base := t.DataDir
	if base == "" {
		base = os.TempDir()
	} else {
		stale, _ := filepath.Glob(filepath.Join(base, "data-dir-*"))
		for _, d := range stale {
			_ = os.RemoveAll(d)
		}
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	exe := t.exe()
	quiet := process.CmdCreatorFunc(func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, exe, args...), nil // #nosec G204 -- the tor binary the user configured
	})
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	proc, kill, err := launch(bctx, &tor.StartConf{ProcessCreator: quiet, TempDataDirBase: base})
	if err != nil {
		return err
	}
	proc.DeleteDataDirOnClose = true
	proc.StopProcessOnClose = true
	d, err := proc.Dialer(bctx, nil)
	if err != nil {
		_ = proc.Close()
		kill()
		return err
	}
	t.proc, t.kill, t.dialer = proc, kill, d
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
	t.mu.Lock()
	proc := t.proc
	t.mu.Unlock()
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	svc, err := proc.Listen(bctx, &tor.ListenConf{Key: t.Key, RemotePorts: []int{int(t.Port)}, LocalListener: local, Version3: true})
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
	t.mu.Lock()
	d := t.dialer
	t.mu.Unlock()
	if d == nil {
		return nil, errors.New("tor: closed")
	}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(addr.Host, strconv.Itoa(int(addr.Port))))
}

// Close stops tor, removes its data directory and wipes the onion key. The
// copy tor.Listen handed to the library stays inside the library until it is
// collected (SECURITY.md).
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.Key)
	if t.onion != nil {
		_ = t.onion.Close()
		t.onion = nil
	}
	if t.proc == nil {
		return nil
	}
	err := t.proc.Close()
	t.kill()
	t.proc, t.dialer = nil, nil
	return err
}
