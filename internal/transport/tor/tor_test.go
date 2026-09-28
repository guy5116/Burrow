package tor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"testing"

	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/transport/conformance"
)

// TestConformance needs a tor binary and network access to the Tor network;
// it runs only with BURROW_TOR_TEST=1.
func TestConformance(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	tr := New(key, 47337, "")
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
	tr := New(key, 1, "/definitely/not/tor")
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
}
