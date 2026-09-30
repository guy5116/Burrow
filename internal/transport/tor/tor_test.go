package tor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
