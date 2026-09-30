package core

import (
	"crypto/ed25519"
	"os"
	"path/filepath"

	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/tcp"
	"github.com/guy5116/burrow/internal/transport/tor"
	"github.com/guy5116/burrow/internal/wire"
)

// Transport is re-exported so UIs can hold the result of Transports without
// importing internal/transport.
type Transport = transport.Transport

// TorOptions enables the onion transport.
type TorOptions struct {
	Key  ed25519.PrivateKey // from store.LoadOnionKey
	Port uint16             // virtual port in invites (0 → listen port)
	Exe  string             // tor binary ("" → PATH)
	// DataDir is the application's data directory; tor's state goes in a
	// private sub-directory of it.
	DataDir string
}

// Transports returns the transports for a listen address (":port" or
// "host:port") and, when opts is non-nil, the Tor transport. UIs pass the
// result to New without importing transport packages. Pass listenAddr "" to
// run Tor only.
func Transports(listenAddr string, opts *TorOptions) []transport.Transport {
	var out []transport.Transport
	if listenAddr != "" {
		out = append(out, tcp.New(listenAddr))
	}
	if opts != nil {
		port := opts.Port
		if port == 0 {
			port = wire.DefaultListenPort
		}
		dir := ""
		if opts.DataDir != "" {
			dir = filepath.Join(opts.DataDir, "tor")
		}
		out = append(out, tor.New(opts.Key, port, opts.Exe, dir))
	}
	return out
}

// OnionAddress returns this instance's onion address ("" when Tor is not running).
func (e *Engine) OnionAddress() string {
	if t, ok := e.trs[transport.KindTor].(*tor.Transport); ok {
		return t.OnionAddress()
	}
	return ""
}

func listParts(dir string) ([]string, error) { return filepath.Glob(filepath.Join(dir, "*.part")) }

func removeFile(p string) error { return os.Remove(p) }
