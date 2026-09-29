package invite

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/guy5116/burrow/internal/identity"
)

func sample() *Invite {
	inv := &Invite{Kind: KindTCP, Addr: "192.0.2.10", Port: 47337, Expiry: time.Unix(1_800_000_000, 0)}
	for i := range inv.PubKey {
		inv.PubKey[i] = byte(i + 1)
	}
	for i := range inv.Token {
		inv.Token[i] = byte(0xF0 + i)
	}
	return inv
}

func TestRoundTrip(t *testing.T) {
	for _, in := range []*Invite{
		sample(),
		func() *Invite { i := sample(); i.MultiUse = true; i.Addr = "chat.example"; return i }(),
		func() *Invite { i := sample(); i.Addr = "2001:db8::1"; return i }(),
		func() *Invite {
			i := sample()
			i.Kind = KindTor
			i.Addr = strings.Repeat("a", 56) + ".onion"
			return i
		}(),
		func() *Invite {
			i := sample()
			i.Addr = strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
			return i
		}(),
	} {
		s, err := in.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s, Prefix) || s != strings.ToLower(s) {
			t.Fatalf("%q", s)
		}
		got, err := Parse("  " + s + "\n")
		if err != nil {
			t.Fatal(err)
		}
		if *got != *in {
			t.Fatalf("got %+v want %+v", got, in)
		}
		if !got.Expired(in.Expiry) || got.Expired(in.Expiry.Add(-time.Second)) {
			t.Fatal("Expired")
		}
	}
}

func TestEncodeRejects(t *testing.T) {
	mods := map[string]func(*Invite){
		"port0":       func(i *Invite) { i.Port = 0 },
		"empty-addr":  func(i *Invite) { i.Addr = "" },
		"long-addr":   func(i *Invite) { i.Addr = strings.Repeat("a", 254) },
		"zero-key":    func(i *Invite) { i.PubKey = identity.PeerID{} },
		"bad-kind":    func(i *Invite) { i.Kind = 3 },
		"upper-host":  func(i *Invite) { i.Addr = "Chat.example" },
		"space-host":  func(i *Invite) { i.Addr = "chat example" },
		"dash-label":  func(i *Invite) { i.Addr = "-chat.example" },
		"long-label":  func(i *Invite) { i.Addr = strings.Repeat("a", 64) + ".example" },
		"empty-label": func(i *Invite) { i.Addr = "chat..example" },
		"tor-not-onion": func(i *Invite) {
			i.Kind = KindTor
			i.Addr = "chat.example"
		},
		"tor-v2-len": func(i *Invite) {
			i.Kind = KindTor
			i.Addr = strings.Repeat("a", 16) + ".onion"
		},
		"expiry0": func(i *Invite) { i.Expiry = time.Unix(0, 0) },
	}
	for name, mod := range mods {
		i := sample()
		mod(i)
		if _, err := i.Encode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	s, _ := sample().Encode()
	body := s[len(Prefix):]
	bad := []string{
		"", "burrow1", "burrow2:" + body, strings.ToUpper(s), s[:len(s)-1], s + "a", s + "aaaaaaaa",
		Prefix, Prefix + "!!!!!!!!", Prefix + body[:len(body)-2] + "zz", s + " x",
	}
	// flip one character in every position; the checksum must catch it
	for i := len(Prefix); i < len(s); i++ {
		c := byte('a')
		if s[i] == 'a' {
			c = 'b'
		}
		bad = append(bad, s[:i]+string(c)+s[i+1:])
	}
	for _, b := range bad {
		if _, err := Parse(b); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %q", b)
		}
	}
	if errors.Is(ErrInvalid, nil) || strings.Contains(ErrInvalid.Error(), body[:8]) {
		t.Fatal("error leaks invite")
	}
}

// Structural cases the checksum alone would not catch: build a raw payload
// with a valid check but bad semantics.
func TestParseStructural(t *testing.T) {
	raw := func(mod func([]byte) []byte) string {
		i := sample()
		s, _ := i.Encode()
		b, _ := b32.DecodeString(strings.ToUpper(s[len(Prefix):]))
		b = mod(b[:len(b)-checkLen])
		c := check(b)
		return Prefix + strings.ToLower(b32.EncodeToString(append(b, c[:]...)))
	}
	cases := map[string]string{
		"reserved-flag": raw(func(b []byte) []byte { b[len(b)-1] = 2; return b }),
		"version2":      raw(func(b []byte) []byte { b[0] = 2; return b }),
		"kind0":         raw(func(b []byte) []byte { b[1] = 0; return b }),
		"port0":         raw(func(b []byte) []byte { b[3+10], b[3+11] = 0, 0; return b }),
		"addrlen-lie":   raw(func(b []byte) []byte { b[2] = 11; return b }),
		"addrlen-254":   raw(func(b []byte) []byte { b[2] = 254; return append(b, make([]byte, 244)...) }),
		"trailing":      raw(func(b []byte) []byte { return append(b, 0) }),
	}
	for name, s := range cases {
		if _, err := Parse(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted", name)
		}
	}
}

func FuzzInviteParse(f *testing.F) {
	s, _ := sample().Encode()
	f.Add(s)
	f.Add(Prefix)
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		inv, err := Parse(s)
		if err != nil {
			return
		}
		re, err := inv.Encode()
		if err != nil || re != strings.TrimSpace(s) {
			t.Fatalf("re-encode mismatch")
		}
	})
}

func TestParseBytes(t *testing.T) {
	s, _ := sample().Encode()
	in := []byte("  " + s + "\n")
	got, err := ParseBytes(in)
	if err != nil || *got != *sample() {
		t.Fatal(got, err)
	}
	if string(in) != "  "+s+"\n" {
		t.Fatal("ParseBytes must not modify its input")
	}
	for _, bad := range [][]byte{nil, []byte("burrow1:"), []byte(strings.ToUpper(s)), []byte(s + "="), []byte(s[:len(s)-1] + "1"), []byte(s[:len(s)-1] + "!")} {
		if _, err := ParseBytes(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %q", bad)
		}
	}
}
