// Package conformance is the test suite every Transport must pass.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/transport"
)

// Run exercises tr. addrFor converts a listener's net.Addr into the Address
// a peer would dial (e.g. from an invite).
func Run(t *testing.T, tr transport.Transport, addrFor func(net.Addr) transport.Address) {
	t.Helper()
	if tr.Kind() == "" {
		t.Fatal("empty Kind")
	}
	ctx := context.Background()
	ln, err := tr.Listen(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := addrFor(ln.Addr())

	t.Run("dial and exchange", func(t *testing.T) {
		accepted := make(chan net.Conn, 1)
		go func() {
			c, err := ln.Accept()
			if err != nil {
				t.Error(err)
				accepted <- nil
				return
			}
			accepted <- c
		}()
		c, err := tr.Dial(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		s := <-accepted
		if s == nil {
			t.Fatal("accept failed")
		}
		defer func() { _ = s.Close() }()
		k1 := tr.RateKey(s.RemoteAddr())
		if k2 := tr.RateKey(s.RemoteAddr()); k1 != k2 {
			t.Fatal("RateKey not deterministic")
		}
		msg := bytes.Repeat([]byte{0xAB}, 70000)
		go func() { _, _ = c.Write(msg) }()
		got := make([]byte, len(msg))
		_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := readFull(s, got); err != nil || !bytes.Equal(got, msg) {
			t.Fatal("client→server", err)
		}
		go func() { _, _ = s.Write(msg[:1000]) }()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := readFull(c, got[:1000]); err != nil {
			t.Fatal("server→client", err)
		}
		// Deadlines are honored.
		_ = c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		var ne net.Error
		if _, err := c.Read(got); err == nil {
			t.Fatal("expected deadline error")
		} else if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("not a timeout: %v", err)
		}
		// Close is observed by the other side.
		_ = s.Close()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(got); err == nil {
			t.Fatal("read after peer close succeeded")
		}
	})

	t.Run("dial cancelled context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if c, err := tr.Dial(cctx, addr); err == nil {
			_ = c.Close()
			t.Fatal("dial with cancelled context succeeded")
		}
	})

	t.Run("dial wrong kind", func(t *testing.T) {
		bad := addr
		bad.Kind = "nope"
		if c, err := tr.Dial(ctx, bad); err == nil {
			_ = c.Close()
			t.Fatal("dial with wrong kind succeeded")
		}
	})

	// Last, because it closes the listener: a transport need not listen
	// twice (Tor publishes one onion service per key).
	t.Run("close unblocks accept", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { _, err := ln.Accept(); done <- err }()
		time.Sleep(20 * time.Millisecond)
		_ = ln.Close()
		select {
		case err := <-done:
			if err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("accept returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Accept did not unblock on Close")
		}
	})
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
