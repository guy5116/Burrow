// Package tor is the onion-service transport (CLAUDE.md §5, Phase 4). It
// controls a system `tor` binary through github.com/cretz/bine; Tor is never
// bundled. The onion service's ed25519 key is a second long-term secret that
// lives in the encrypted identity blob and reaches tor only through the
// control port (ADD_ONION), never through tor's data directory.
package tor

import (
	"context"
	"crypto/ed25519"
	"crypto/sha3"
	"encoding/base32"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// DataDir is tor's state directory. It is kept from one run to the next,
	// so that tor keeps its entry guards (a new guard on every start is a new
	// chance for someone who runs relays to become it). The onion key never
	// goes there: it reaches tor over the control port. "" → a temporary
	// directory that is removed on Close (tests).
	DataDir string
	// Bootstrap bounds how long starting tor and publishing the service may take.
	Bootstrap time.Duration

	startMu sync.Mutex // one start at a time; held while tor bootstraps, never by readers

	mu      sync.Mutex // guards the fields below; never held for long
	closed  bool
	proc    *tor.Tor
	kill    context.CancelFunc // ends the tor process
	dialer  *tor.Dialer
	onion   *tor.OnionService
	stop    context.Context // ends when Close is called, and with it a start in progress
	onStop  context.CancelFunc
	onionID atomic.Pointer[string]
}

// ErrNoTor is returned when no tor binary can be found.
var ErrNoTor = errors.New("tor: no tor binary found (install tor or set tor_exe)")

var errClosed = errors.New("tor: closed")

// New builds a transport for the given onion key and virtual port.
func New(key ed25519.PrivateKey, port uint16, exePath, dataDir string) *Transport {
	t := &Transport{Key: key, Port: port, ExePath: exePath, DataDir: dataDir, Bootstrap: 3 * time.Minute}
	t.stop, t.onStop = context.WithCancel(context.Background())
	return t
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

// start launches tor once and waits for bootstrap. tor's own output is
// discarded: it must reach neither the chat nor the JSON event stream. tor
// exits by itself when this process dies, however it dies.
func (t *Transport) start(ctx context.Context) error {
	t.startMu.Lock()
	defer t.startMu.Unlock()
	t.mu.Lock()
	closed, running := t.closed, t.proc != nil
	t.mu.Unlock()
	switch {
	case closed:
		return errClosed
	case running:
		return nil
	case !t.Available():
		return ErrNoTor
	}
	conf := &tor.StartConf{ExtraArgs: []string{"__OwningControllerProcess", strconv.Itoa(os.Getpid())}}
	exe := t.exe()
	conf.ProcessCreator = process.CmdCreatorFunc(func(ctx context.Context, args ...string) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, exe, args...), nil // #nosec G204 G702 -- the tor binary the user configured
	})
	if t.DataDir == "" {
		conf.TempDataDirBase = os.TempDir()
	} else {
		if err := os.MkdirAll(t.DataDir, 0o700); err != nil { // #nosec G703 -- a directory inside our own data directory
			return err
		}
		// The control library writes a torrc and a port file for every run and
		// leaves them behind; tor's own state stays.
		for _, pattern := range []string{"torrc-*", "control-port-*"} {
			stale, _ := filepath.Glob(filepath.Join(t.DataDir, pattern))
			for _, f := range stale {
				_ = os.Remove(f) // #nosec G703 -- the control library's files, inside that directory
			}
		}
		conf.DataDir = t.DataDir
	}
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	defer context.AfterFunc(t.stop, cancel)()
	proc, kill, err := launch(bctx, conf)
	if err != nil {
		return err
	}
	proc.DeleteDataDirOnClose = t.DataDir == ""
	proc.StopProcessOnClose = true
	d, err := proc.Dialer(bctx, nil)
	if err == nil {
		t.mu.Lock()
		if t.closed {
			err = errClosed
		} else {
			t.proc, t.kill, t.dialer = proc, kill, d
		}
		t.mu.Unlock()
	}
	if err != nil {
		_ = proc.Close()
		kill()
	}
	return err
}

// Listen publishes the onion service and returns its listener. The onion
// address is available afterwards through OnionAddress. There is one service
// per key: a second Listen is refused.
func (t *Transport) Listen(ctx context.Context) (transport.Listener, error) {
	if err := t.start(ctx); err != nil {
		return nil, err
	}
	t.mu.Lock()
	proc, listening := t.proc, t.onion != nil
	t.mu.Unlock()
	if proc == nil {
		return nil, errClosed
	}
	if listening {
		return nil, errors.New("tor: already listening")
	}
	local, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	bctx, cancel := context.WithTimeout(ctx, t.Bootstrap)
	defer cancel()
	defer context.AfterFunc(t.stop, cancel)()
	svc, err := proc.Listen(bctx, &tor.ListenConf{Key: t.Key, RemotePorts: []int{int(t.Port)}, LocalListener: local, Version3: true})
	if err != nil {
		_ = local.Close()
		return nil, err
	}
	svc.CloseLocalListenerOnClose = true
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.onion != nil {
		_ = svc.Close()
		return nil, errClosed
	}
	t.onion = svc
	id := svc.ID + ".onion"
	t.onionID.Store(&id)
	return svc, nil
}

// OnionAddress returns "<56 chars>.onion" once Listen succeeded ("" before).
// It never waits, not even for a tor that is still starting.
func (t *Transport) OnionAddress() string {
	if id := t.onionID.Load(); id != nil {
		return *id
	}
	return ""
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

// Close stops tor (and a start in progress) and wipes the onion key. The
// copy tor.Listen handed to the library stays inside the library until it is
// collected (SECURITY.md). A closed transport does not start again.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.onStop()
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

// Address returns the onion address of key without running tor: the
// address is the key's public half, a checksum and a version byte, in
// base32 (rend-spec-v3 §6).
func Address(key ed25519.PrivateKey) string {
	pub := key.Public().(ed25519.PublicKey)
	sum := sha3.Sum256(append(append([]byte(".onion checksum"), pub...), 3))
	raw := append(append(append([]byte(nil), pub...), sum[:2]...), 3)
	return strings.ToLower(base32.StdEncoding.EncodeToString(raw)) + ".onion"
}
