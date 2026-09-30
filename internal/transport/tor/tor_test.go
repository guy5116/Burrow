package tor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cretz/bine/tor"

	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/conformance"
)

// TestConformance needs a tor binary and network access to the Tor network;
// it runs only with BURROW_TOR_TEST=1.
func TestConformance(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tr := New(key, 47337, "", t.TempDir())
	if os.Getenv("BURROW_TOR_TEST") == "" || !tr.Available() {
		t.Skip("set BURROW_TOR_TEST=1 with tor installed")
	}
	defer tr.Close()
	conformance.Run(t, tr, func(net.Addr) transport.Address {
		return transport.Address{Kind: transport.KindTor, Host: tr.OnionAddress(), Port: 47337}
	})
}

func TestWithoutTor(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tr := New(key, 1, "/definitely/not/tor", "")
	if tr.Available() {
		t.Skip("unexpected tor at that path")
	}
	if _, err := tr.Listen(context.Background()); err == nil {
		t.Fatal("Listen without tor succeeded")
	}
	if _, err := tr.Dial(context.Background(), transport.Address{Kind: transport.KindTor, Host: "x.onion", Port: 1}); err == nil {
		t.Fatal("Dial without tor succeeded")
	}
	if _, err := tr.Dial(context.Background(), transport.Address{Kind: transport.KindTCP, Host: "x", Port: 1}); err == nil {
		t.Fatal("wrong kind accepted")
	}
	if tr.Kind() != transport.KindTor || tr.RateKey(nil) != "" || tr.OnionAddress() != "" {
		t.Fatal("accessors")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("onion key not wiped by Close")
	}
}

// tor outlives the context that bounded its start, and dies with it when the
// start did not finish in time.
func TestTorOutlivesItsStart(t *testing.T) {
	defer func() { startTor = tor.Start }()
	var life context.Context
	startTor = func(ctx context.Context, _ *tor.StartConf) (*tor.Tor, error) {
		life = ctx
		return &tor.Tor{}, nil
	}
	start, cancel := context.WithCancel(context.Background())
	_, kill, err := launch(start, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the bootstrap deadline passing, or the caller giving up afterwards
	time.Sleep(20 * time.Millisecond)
	if life.Err() != nil {
		t.Fatal("tor was killed when the context of its start ended")
	}
	kill()
	if life.Err() == nil {
		t.Fatal("kill does not end the process")
	}

	startTor = func(ctx context.Context, _ *tor.StartConf) (*tor.Tor, error) {
		life = ctx
		<-ctx.Done() // still starting when the deadline passes
		return nil, ctx.Err()
	}
	start, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := launch(start, nil); err == nil || life.Err() == nil {
		t.Fatal("a start that ran out of time left tor running", err)
	}
}

// The address is computed from the key alone, as tor computes it. The vector
// is tor's own (test_hs_common.c, test_build_address).
func TestAddress(t *testing.T) {
	pub, _ := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	copy(key[32:], pub) // Address reads only the public half
	if got := Address(key); got != "25njqamcweflpvkl73j4szahhihoc4xt3ktcgjnpaingr5yhkenl5sid.onion" {
		t.Fatal(got)
	}
}

// Readers never wait for a tor that is still starting, and Close stops it.
func TestStartDoesNotBlockReaders(t *testing.T) {
	defer func() { startTor = tor.Start }()
	started := make(chan struct{})
	startTor = func(ctx context.Context, _ *tor.StartConf) (*tor.Tor, error) {
		close(started)
		<-ctx.Done() // bootstrapping for ever
		return nil, ctx.Err()
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tr := New(key, 1, os.Args[0], t.TempDir()) // any file that exists will do as the binary
	errc := make(chan error, 1)
	go func() { _, err := tr.Listen(context.Background()); errc <- err }()
	<-started
	done := make(chan struct{})
	go func() { _ = tr.OnionAddress(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnionAddress waited for the start")
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Listen succeeded after Close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not stop the start")
	}
	if _, err := tr.Dial(context.Background(), transport.Address{Kind: transport.KindTor, Host: "x.onion", Port: 1}); !errors.Is(err, errClosed) {
		t.Fatal("a closed transport started again", err)
	}
}
