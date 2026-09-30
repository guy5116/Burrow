package mdns

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
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

func TestParseIgnoresForeignAndMalformedRecords(t *testing.T) {
	build := func(fn func(b *dnsmessage.Builder)) []byte {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true})
		_ = b.StartQuestions()
		_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName("x.local."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
		_ = b.StartAnswers()
		fn(&b)
		p, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	inst := dnsmessage.MustNewName("abc._burrow._tcp.local.")
	host := dnsmessage.MustNewName("abc.local.")
	hdr := func(n dnsmessage.Name, typ dnsmessage.Type) dnsmessage.ResourceHeader {
		return dnsmessage.ResourceHeader{Name: n, Type: typ, Class: dnsmessage.ClassINET, TTL: 60}
	}
	nonce, tag := "n="+hex32(1), "t="+hex32(2)
	good := build(func(b *dnsmessage.Builder) {
		_ = b.AResource(hdr(host, dnsmessage.TypeA), dnsmessage.AResource{A: [4]byte{10, 0, 0, 1}}) // foreign name: skipped
		_ = b.AResource(hdr(inst, dnsmessage.TypeA), dnsmessage.AResource{A: [4]byte{10, 0, 0, 2}}) // our name, other type: skipped
		_ = b.SRVResource(hdr(inst, dnsmessage.TypeSRV), dnsmessage.SRVResource{Port: 9, Target: host})
		_ = b.TXTResource(hdr(inst, dnsmessage.TypeTXT), dnsmessage.TXTResource{TXT: []string{"junk", nonce, tag}})
	})
	an, ok := Parse(good, net.IPv4(1, 2, 3, 4))
	if !ok || an.Port != 9 || an.Name != "abc" || an.Nonce[0] != 1 || an.Tag[0] != 2 {
		t.Fatalf("%+v %v", an, ok)
	}
	for name, pkt := range map[string][]byte{
		"no txt": build(func(b *dnsmessage.Builder) {
			_ = b.SRVResource(hdr(inst, dnsmessage.TypeSRV), dnsmessage.SRVResource{Port: 9, Target: host})
		}),
		"no srv": build(func(b *dnsmessage.Builder) {
			_ = b.TXTResource(hdr(inst, dnsmessage.TypeTXT), dnsmessage.TXTResource{TXT: []string{nonce, tag}})
		}),
		"port zero": build(func(b *dnsmessage.Builder) {
			_ = b.SRVResource(hdr(inst, dnsmessage.TypeSRV), dnsmessage.SRVResource{Port: 0, Target: host})
			_ = b.TXTResource(hdr(inst, dnsmessage.TypeTXT), dnsmessage.TXTResource{TXT: []string{nonce, tag}})
		}),
		"bad hex": build(func(b *dnsmessage.Builder) {
			_ = b.SRVResource(hdr(inst, dnsmessage.TypeSRV), dnsmessage.SRVResource{Port: 9, Target: host})
			_ = b.TXTResource(hdr(inst, dnsmessage.TypeTXT), dnsmessage.TXTResource{TXT: []string{"n=" + strings.Repeat("z", 32), tag}})
		}),
		"missing tag": build(func(b *dnsmessage.Builder) {
			_ = b.SRVResource(hdr(inst, dnsmessage.TypeSRV), dnsmessage.SRVResource{Port: 9, Target: host})
			_ = b.TXTResource(hdr(inst, dnsmessage.TypeTXT), dnsmessage.TXTResource{TXT: []string{nonce}})
		}),
	} {
		if _, ok := Parse(pkt, nil); ok {
			t.Errorf("%s accepted", name)
		}
	}
	if _, ok := Parse(good[:len(good)-5], nil); ok {
		t.Fatal("truncated record accepted")
	}
}

func hex32(b byte) string { return hex.EncodeToString(append([]byte{b}, make([]byte, 15)...)) }

func TestRunAndBrowseStopOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var n, tg [16]byte
	a, _ := NewAnnouncer(1, n, tg)
	done := make(chan error, 2)
	go func() { done <- a.Run(ctx) }()
	go func() { done <- Browse(ctx, func(Announcement) {}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	for i := 0; i < 2; i++ {
		select {
		case <-done: // nil, or a bind error where multicast is unavailable: both mean "stopped"
		case <-time.After(3 * time.Second):
			t.Fatal("did not stop on cancel")
		}
	}
}

// TestWriteFuzzSeeds regenerates testdata/fuzz (run with BURROW_WRITE_SEEDS=1).
func TestWriteFuzzSeeds(t *testing.T) {
	if os.Getenv("BURROW_WRITE_SEEDS") == "" {
		t.Skip("set BURROW_WRITE_SEEDS=1 to regenerate the seed corpus")
	}
	var n, tg [16]byte
	n[0], tg[0] = 1, 2
	a, _ := NewAnnouncer(47337, n, tg)
	a.name = "0123456789abcdef"
	pkt, _ := a.Packet()
	dir := filepath.Join("testdata", "fuzz", "FuzzParse")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "go test fuzz v1\n[]byte(" + strconv.Quote(string(pkt)) + ")\n"
	if err := os.WriteFile(filepath.Join(dir, "seed-0"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseBudget(t *testing.T) {
	var b budget
	now := time.Unix(100, 0)
	passed := 0
	for n := 0; n < 10*browseBudget; n++ {
		if b.take(now.Add(time.Duration(n) * time.Millisecond)) { // all within one second
			passed++
		}
	}
	if passed != browseBudget {
		t.Fatalf("%d announcements passed in one second", passed)
	}
	if !b.take(now.Add(time.Second)) {
		t.Fatal("the budget did not refill")
	}
}
