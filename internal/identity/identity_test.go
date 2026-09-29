package identity

import (
	"bytes"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 7748 §6.1 known answer.
func TestX25519KAT(t *testing.T) {
	scalar := unhex(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	id, err := FromScalar(scalar)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(scalar, make([]byte, 32)) {
		t.Fatal("FromScalar must wipe input")
	}
	want := unhex(t, "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	pub := id.Public()
	if !bytes.Equal(pub[:], want) {
		t.Fatalf("pub %x", pub)
	}
	if bytes.Equal(id.Scalar(), make([]byte, 32)) {
		t.Fatal("scalar lost")
	}
	id.Clear()
	if !bytes.Equal(id.Scalar(), make([]byte, 32)) {
		t.Fatal("Clear")
	}
	if _, err := FromScalar([]byte{1, 2}); err == nil {
		t.Fatal("short scalar accepted")
	}
}

func TestGenerate(t *testing.T) {
	a, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate()
	if a.Public() == b.Public() {
		t.Fatal("two identities equal")
	}
}

func TestFingerprint(t *testing.T) {
	var zero PeerID
	fp := zero.Fingerprint()
	if fp != strings.Repeat("a", 52) {
		t.Fatalf("%q", fp)
	}
	var p PeerID
	for i := range p {
		p[i] = byte(i * 7)
	}
	fp = p.Fingerprint()
	if len(fp) != FingerprintLen || !regexp.MustCompile(`^[a-z2-7]{52}$`).MatchString(fp) {
		t.Fatalf("%q", fp)
	}
	if p.Short() != fp[:8] {
		t.Fatal("short")
	}
	disp := p.Display()
	if len(disp) != 52+12 || strings.ReplaceAll(disp, " ", "") != fp {
		t.Fatalf("%q", disp)
	}
	for _, in := range []string{fp, disp, strings.ToUpper(fp), strings.ReplaceAll(disp, " ", "-")} {
		got, err := ParseFingerprint(in)
		if err != nil || got != p {
			t.Fatalf("parse %q: %v", in, err)
		}
	}
	for _, in := range []string{"", fp[:51], fp + "a", strings.Replace(fp, "a", "1", 1), strings.Repeat("z", 52)} {
		if _, err := ParseFingerprint(in); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
}

func TestSafetyNumber(t *testing.T) {
	var a, b PeerID
	a[0], b[0] = 1, 2
	x, y := ComputeSafetyNumber(a, b), ComputeSafetyNumber(b, a)
	if x != y {
		t.Fatal("not symmetric")
	}
	if x == ComputeSafetyNumber(a, a) {
		t.Fatal("collision")
	}
	s := x.String()
	if !regexp.MustCompile(`^(\d{5} ){11}\d{5}$`).MatchString(s) {
		t.Fatalf("%q", s)
	}
	for _, g := range x {
		if g >= 100000 {
			t.Fatal("group range")
		}
	}
	// Pinned answer so both UIs and any reimplementation agree.
	if s != knownSafety {
		t.Fatalf("safety number changed: %s", s)
	}
}

// knownSafety is ComputeSafetyNumber for keys {1,0,...} and {2,0,...},
// recorded from the reference implementation of §3.3.
const knownSafety = "84418 61305 05322 46155 37396 02520 58919 13659 77920 49724 23922 76258"

func TestMDNSTag(t *testing.T) {
	var n [16]byte
	var p PeerID
	p[3] = 9
	t1 := MDNSTag(n, p)
	n[0] = 1
	t2 := MDNSTag(n, p)
	if t1 == t2 || t1 == [16]byte{} {
		t.Fatal("tag")
	}
	if a := testing.AllocsPerRun(10, func() { MDNSTag(n, p) }); a > 2 {
		t.Logf("MDNSTag allocs %v", a)
	}
}

func TestStorageLifecycle(t *testing.T) {
	src := unhex(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	want := append([]byte(nil), src...)
	id, err := FromScalar(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(src, make([]byte, 32)) {
		t.Fatal("input not wiped")
	}
	if !bytes.Equal(id.Scalar(), want) {
		t.Fatal("scalar not preserved in storage")
	}
	// The key must stay usable for many reads (handshakes and rekeys read it repeatedly).
	for i := 0; i < 100; i++ {
		if id.Scalar()[0] != want[0] {
			t.Fatal("storage changed")
		}
	}
	id.Clear()
	id.Clear() // idempotent; with memguard the storage is unmapped, Scalar must not touch it
	if !bytes.Equal(id.Scalar(), make([]byte, 32)) {
		t.Fatal("scalar readable after Clear")
	}
	t.Logf("identity keys in locked memory: %v", Locked())
}
