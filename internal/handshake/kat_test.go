package handshake

import (
	"bufio"
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/flynn/noise"

	"github.com/guy5116/burrow/internal/wire"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 5869 A.1 and A.2.
func TestHKDFVectors(t *testing.T) {
	cases := []struct{ ikm, salt, info, prk, okm string }{
		{"0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b", "000102030405060708090a0b0c", "f0f1f2f3f4f5f6f7f8f9",
			"077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5",
			"3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"},
		{"000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f",
			"606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeaf",
			"b0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeeff0f1f2f3f4f5f6f7f8f9fafbfcfdfeff",
			"06a6b88c5853361a06104c9ceb35b45cef760014904671014a193f40c15fc244",
			"b11e398dc80327a1c8e7f78c596a49344f012eda2d4efad8a050cc4c19afa97c59045a99cac7827271cb41c65e590e09da3275600c2f09b8367793a9aca3db71cc30c58179ec3e87c14c01d5c1f3434f1d87"},
	}
	for i, c := range cases {
		prk, err := hkdf.Extract(sha256.New, unhex(t, c.ikm), unhex(t, c.salt))
		if err != nil || !bytes.Equal(prk, unhex(t, c.prk)) {
			t.Fatalf("case %d prk: %x %v", i, prk, err)
		}
		want := unhex(t, c.okm)
		okm, err := hkdf.Expand(sha256.New, prk, string(unhex(t, c.info)), len(want))
		if err != nil || !bytes.Equal(okm, want) {
			t.Fatalf("case %d okm: %x %v", i, okm, err)
		}
	}
}

// RFC 4231 test cases 1, 2, 6 for HMAC-SHA-256.
func TestHMACVectors(t *testing.T) {
	cases := []struct{ key, data, mac string }{
		{"0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b", hex.EncodeToString([]byte("Hi There")),
			"b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7"},
		{hex.EncodeToString([]byte("Jefe")), hex.EncodeToString([]byte("what do ya want for nothing?")),
			"5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"},
		{strings.Repeat("aa", 131), hex.EncodeToString([]byte("Test Using Larger Than Block-Size Key - Hash Key First")),
			"60e431591ee0b67f0d8a26aacbf5b77f8e0bc6213728c5140546040f0ee37f54"},
	}
	for i, c := range cases {
		m := hmac.New(sha256.New, unhex(t, c.key))
		m.Write(unhex(t, c.data))
		if got := m.Sum(nil); !bytes.Equal(got, unhex(t, c.mac)) {
			t.Fatalf("case %d: %x", i, got)
		}
	}
}

// ML-KEM-768: sizes match the wire constants, encaps/decaps round-trips, a
// fixed seed is deterministic, an invalid encapsulation key is rejected, and a
// tampered ciphertext yields a different secret (implicit rejection, no error).
func TestMLKEM768(t *testing.T) {
	seed := make([]byte, wire.KEMSeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	dk, err := mlkem.NewDecapsulationKey768(seed)
	if err != nil {
		t.Fatal(err)
	}
	ekBytes := dk.EncapsulationKey().Bytes()
	if len(ekBytes) != wire.KEMEncapsulationKeySize {
		t.Fatalf("ek size %d", len(ekBytes))
	}
	dk2, _ := mlkem.NewDecapsulationKey768(seed)
	if !bytes.Equal(dk2.EncapsulationKey().Bytes(), ekBytes) {
		t.Fatal("seed not deterministic")
	}
	if got := sha256.Sum256(ekBytes); hex.EncodeToString(got[:]) != mlkemSeedPin {
		t.Fatalf("pinned ek digest changed: %x", got)
	}
	ek, err := mlkem.NewEncapsulationKey768(ekBytes)
	if err != nil {
		t.Fatal(err)
	}
	ss, ct := ek.Encapsulate()
	if len(ss) != wire.KEMSharedSecretSize || len(ct) != wire.KEMCiphertextSize {
		t.Fatalf("sizes %d %d", len(ss), len(ct))
	}
	got, err := dk.Decapsulate(ct)
	if err != nil || !bytes.Equal(got, ss) {
		t.Fatalf("decaps: %v", err)
	}
	ct[0] ^= 1
	got2, err := dk.Decapsulate(ct)
	if err != nil || bytes.Equal(got2, ss) {
		t.Fatal("tampered ciphertext must implicitly reject")
	}
	if _, err := mlkem.NewEncapsulationKey768(ekBytes[:len(ekBytes)-1]); err == nil {
		t.Fatal("short ek accepted")
	}
	bad := append([]byte(nil), ekBytes...)
	for i := range bad[:1152] { // out-of-range polynomial coefficients
		bad[i] = 0xff
	}
	if _, err := mlkem.NewEncapsulationKey768(bad); err == nil {
		t.Fatal("non-canonical ek accepted")
	}
	if _, err := mlkem.NewDecapsulationKey768(seed[:63]); err == nil {
		t.Fatal("short seed accepted")
	}
}

// mlkemSeedPin is SHA-256 of the encapsulation key derived from seed 0..63;
// pinned so an upstream change in seed expansion is noticed.
const mlkemSeedPin = "0b7934c83125c788995e2ba6bd761e33046b3e40571be53e023309a29f398cc9"

// Noise XK_25519_ChaChaPoly_BLAKE2s against the cacophony vectors that
// flynn/noise itself tests with (testdata/noise_xk_vectors.txt, extracted from
// its vectors.txt).
func TestNoiseXKVectors(t *testing.T) {
	f, err := os.Open("testdata/noise_xk_vectors.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	var cfgI, cfgR noise.Config
	var hsI, hsR *noise.HandshakeState
	var csI0, csI1, csR0, csR1 *noise.CipherState
	var payload []byte
	blocks := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "handshake":
			if v != "Noise_XK_25519_ChaChaPoly_BLAKE2s" {
				t.Fatalf("unexpected %s", v)
			}
			blocks++
			cfgI = noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK, Initiator: true}
			cfgR = noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK}
			hsI, hsR = nil, nil
		case "init_static":
			cfgI.StaticKeypair, _ = noise.DH25519.GenerateKeypair(bytes.NewReader(unhex(t, v)))
		case "resp_static":
			cfgR.StaticKeypair, _ = noise.DH25519.GenerateKeypair(bytes.NewReader(unhex(t, v)))
			cfgI.PeerStatic = cfgR.StaticKeypair.Public
		case "gen_init_ephemeral":
			cfgI.Random = bytes.NewReader(unhex(t, v))
		case "gen_resp_ephemeral":
			cfgR.Random = bytes.NewReader(unhex(t, v))
		case "prologue":
			cfgI.Prologue = unhex(t, v)
			cfgR.Prologue = cfgI.Prologue
		}
		if !strings.HasPrefix(k, "msg_") {
			continue
		}
		if strings.HasSuffix(k, "_payload") {
			payload = unhex(t, v)
			continue
		}
		if hsI == nil {
			if hsI, err = noise.NewHandshakeState(cfgI); err != nil {
				t.Fatal(err)
			}
			if hsR, err = noise.NewHandshakeState(cfgR); err != nil {
				t.Fatal(err)
			}
		}
		want := unhex(t, v)
		i := int(k[4] - '0')
		if i >= 3 { // transport messages
			enc, dec := csI0, csR0
			if (i-3)%2 == 1 {
				enc, dec = csR1, csI1
			}
			ct, err := enc.Encrypt(nil, nil, payload)
			if err != nil || !bytes.Equal(ct, want) {
				t.Fatalf("block %d msg %d: transport encrypt mismatch", blocks, i)
			}
			pt, err := dec.Decrypt(nil, nil, ct)
			if err != nil || !bytes.Equal(pt, payload) {
				t.Fatalf("block %d msg %d: transport decrypt", blocks, i)
			}
			continue
		}
		w, r := hsI, hsR
		if i%2 == 1 {
			w, r = hsR, hsI
		}
		msg, c0, c1, err := w.WriteMessage(nil, payload)
		if err != nil || !bytes.Equal(msg, want) {
			t.Fatalf("block %d msg %d: %v\n got %x\nwant %x", blocks, i, err, msg, want)
		}
		pt, d0, d1, err := r.ReadMessage(nil, msg)
		if err != nil || !bytes.Equal(pt, payload) {
			t.Fatalf("block %d msg %d read: %v", blocks, i, err)
		}
		if i == 2 {
			csI0, csI1, csR0, csR1 = c0, c1, d0, d1
			if !bytes.Equal(hsI.ChannelBinding(), hsR.ChannelBinding()) {
				t.Fatal("channel binding differs")
			}
			if len(msg) != 32+16+len(payload)+16 {
				t.Fatalf("msg3 len %d", len(msg))
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if blocks != 4 {
		t.Fatalf("expected 4 vector blocks, ran %d", blocks)
	}
}

// Our handshake message sizes follow from the vector-verified pattern: msg1 =
// e || AEAD(payload), msg2 = e || AEAD(payload), msg3 = AEAD(s) || AEAD(payload).
func TestHandshakeSizes(t *testing.T) {
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	sI, _ := suite.GenerateKeypair(nil)
	sR, _ := suite.GenerateKeypair(nil)
	hsI, _ := noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK, Initiator: true,
		Prologue: []byte(wire.Prologue), StaticKeypair: sI, PeerStatic: sR.Public})
	hsR, _ := noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXK,
		Prologue: []byte(wire.Prologue), StaticKeypair: sR})
	m1, _, _, err := hsI.WriteMessage(nil, make([]byte, wire.HS1PayloadLen))
	if err != nil || len(m1) != wire.HS1Len {
		t.Fatalf("msg1 %d %v", len(m1), err)
	}
	if _, _, _, err := hsR.ReadMessage(nil, m1); err != nil {
		t.Fatal(err)
	}
	m2, _, _, _ := hsR.WriteMessage(nil, make([]byte, wire.HS2PayloadLen))
	if len(m2) != wire.HS2Len {
		t.Fatalf("msg2 %d", len(m2))
	}
	if _, _, _, err := hsI.ReadMessage(nil, m2); err != nil {
		t.Fatal(err)
	}
	m3, _, _, _ := hsI.WriteMessage(nil, make([]byte, wire.HS3PayloadLen))
	if len(m3) != wire.HS3Len {
		t.Fatalf("msg3 %d", len(m3))
	}
	if _, _, _, err := hsR.ReadMessage(nil, m3); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hsR.PeerStatic(), sI.Public) {
		t.Fatal("responder learned wrong initiator key")
	}
}
