package session

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

// tcpPair builds two sessions over a loopback TCP connection.
func tcpPair(b *testing.B) (*Session, *Session, func()) {
	b.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	ci, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	cr := <-acc
	_ = ln.Close()
	idI, _ := identity.Generate()
	idR, _ := identity.Generate()
	root, _ := secret.Random(wire.RootKeySize)
	root2 := secret.From(append([]byte(nil), root.Bytes()...))
	ctx, cancel := context.WithCancel(context.Background())
	mk := func(c net.Conn, r *secret.Buffer, init bool, self *identity.Identity, peer identity.PeerID) *Session {
		s, err := New(Config{Conn: c, Root: r, Initiator: init, Self: self, Peer: peer, Logger: slog.New(slog.DiscardHandler), Tick: time.Hour})
		if err != nil {
			b.Fatal(err)
		}
		s.Start(ctx)
		return s
	}
	i, r := mk(ci, root, true, idI, idR.Public()), mk(cr, root2, false, idR, idI.Public())
	<-i.Ready()
	<-r.Ready()
	return i, r, func() { cancel(); <-i.Done(); <-r.Done() }
}

// BenchmarkTextOverSession and BenchmarkTextOverRawTCP together give the added
// latency of a TEXT frame (§11 budget: < 1 ms).
func BenchmarkTextOverSession(b *testing.B) {
	i, r, stop := tcpPair(b)
	defer stop()
	payload, _ := wire.AppendText(nil, wire.Text{MsgID: 1, SentAt: 1, Text: []byte("hello, this is a chat message")})
	ctx := context.Background()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if n%512 == 511 { // stay inside the epoch without involving the controller
			_ = i.Rekey()
		}
		if err := i.Send(ctx, wire.TypeText, payload); err != nil {
			b.Fatal(err)
		}
		<-r.Inbound()
	}
}

func BenchmarkTextOverRawTCP(b *testing.B) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	c, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	s := <-acc
	defer s.Close()
	msg := make([]byte, wire.OuterLenSize+wire.MinCiphertext) // same bytes on the wire as a small frame
	buf := make([]byte, len(msg))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if _, err := c.Write(msg); err != nil {
			b.Fatal(err)
		}
		if _, err := io.ReadFull(s, buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRekey measures one full rekey (3 × X25519 + ML-KEM keygen/encaps/decaps per
// side, both directions switched). §11 budget: < 1.5 ms.
func BenchmarkRekey(b *testing.B) {
	i, r, stop := tcpPair(b)
	defer stop()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		want := uint32(n + 1)
		if err := i.Rekey(); err != nil {
			b.Fatal(err)
		}
		for {
			si, ri := i.Epochs()
			sr, rr := r.Epochs()
			if si == want && ri == want && sr == want && rr == want {
				break
			}
			select {
			case <-i.Done():
				b.Fatal(i.Err())
			default:
			}
			time.Sleep(20 * time.Microsecond)
		}
	}
}
