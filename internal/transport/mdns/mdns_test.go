package mdns

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestPacketRoundTrip(t *testing.T) {
	var nonce, tag [16]byte
	nonce[0], tag[15] = 1, 2
	a, err := NewAnnouncer(47337, nonce, tag)
	if err != nil {
		t.Fatal(err)
	}
	pkt, err := a.Packet()
	if err != nil {
		t.Fatal(err)
	}
	an, ok := Parse(pkt, net.IPv4(192, 0, 2, 1))
	if !ok || an.Port != 47337 || an.Nonce != nonce || an.Tag != tag || an.Name != a.name || !an.IP.Equal(net.IPv4(192, 0, 2, 1)) {
		t.Fatalf("%+v %v", an, ok)
	}
	// A different session gets a different name.
	b, _ := NewAnnouncer(1, nonce, tag)
	if b.name == a.name {
		t.Fatal("name reused")
	}
	query := append([]byte(nil), pkt...)
	query[2] &^= 0x80 // clear the response flag: queries are ignored
	for _, bad := range [][]byte{nil, pkt[:10], query} {
		if _, ok := Parse(bad, nil); ok {
			t.Fatal("malformed accepted")
		}
	}
}

func TestBrowseAnnounceLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var nonce, tag [16]byte
	nonce[3] = 9
	a, _ := NewAnnouncer(4242, nonce, tag)
	got := make(chan Announcement, 4)
	errc := make(chan error, 1)
	go func() { errc <- Browse(ctx, func(an Announcement) { got <- an }) }()
	time.Sleep(100 * time.Millisecond)
	go func() { _ = a.Run(ctx) }()
	select {
	case an := <-got:
		if an.Port != 4242 || an.Nonce != nonce {
			t.Fatalf("%+v", an)
		}
	case err := <-errc:
		t.Skipf("multicast unavailable here: %v", err)
	case <-ctx.Done():
		t.Skip("no multicast loopback in this environment")
	}
}

func FuzzParse(f *testing.F) {
	var n, tg [16]byte
	a, _ := NewAnnouncer(1, n, tg)
	pkt, _ := a.Packet()
	f.Add(pkt)
	f.Fuzz(func(t *testing.T, b []byte) { Parse(b, nil) })
}
