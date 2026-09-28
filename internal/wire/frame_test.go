package wire

import (
	"bytes"
	"errors"
	"testing"

	"github.com/guy5116/burrow/internal/buf"
)

func TestSizesAgree(t *testing.T) {
	if buf.Size != MaxOuterFrame {
		t.Fatalf("buf.Size=%d MaxOuterFrame=%d", buf.Size, MaxOuterFrame)
	}
	if InnerHeaderSize+4+ChunkData != MaxInner {
		t.Fatal("ChunkData must fill a max inner frame exactly")
	}
	if MaxPayload != MaxInner-InnerHeaderSize {
		t.Fatal("MaxPayload")
	}
	if MinCiphertext != 272 || MaxCiphertext != 65552 {
		t.Fatal("ciphertext bounds")
	}
	// Noise message sizes: e(32) + payload + tag.
	if HS1Len != 32+HS1PayloadLen+TagSize || HS2Len != 32+HS2PayloadLen+TagSize || HS3Len != 32+TagSize+HS3PayloadLen+TagSize {
		t.Fatal("handshake message sizes")
	}
}

func TestPaddedLen(t *testing.T) {
	cases := map[int]int{
		0: 256, 1: 256, 247: 256, 248: 256, 249: 512, 504: 512, 505: 768,
		4088: 4096, 4089: 8192, 8184: 8192, 8185: 12288,
		65528: 65536, 65529: 0, -1: 0, 65527: 65536,
	}
	for in, want := range cases {
		if got := PaddedLen(in); got != want {
			t.Errorf("PaddedLen(%d)=%d want %d", in, got, want)
		}
	}
}

func TestOuterLen(t *testing.T) {
	var h [4]byte
	for _, n := range []int{MinCiphertext, 1000, MaxCiphertext} {
		if err := PutOuterLen(h[:], n); err != nil {
			t.Fatal(err)
		}
		if got, err := OuterLen(h[:]); err != nil || got != n {
			t.Fatalf("%d: %v %v", n, got, err)
		}
	}
	for _, n := range []int{0, 1, MinCiphertext - 1, MaxCiphertext + 1, 1 << 31} {
		if err := PutOuterLen(h[:], n); !errors.Is(err, ErrOuterLength) {
			t.Fatalf("PutOuterLen(%d)=%v", n, err)
		}
		h = [4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
		if _, err := OuterLen(h[:]); !errors.Is(err, ErrOuterLength) {
			t.Fatalf("OuterLen(%d)=%v", n, err)
		}
	}
	if _, err := OuterLen(h[:3]); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if err := PutOuterLen(h[:3], 300); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
}

func TestInnerRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte{0xAB}, 300)
	f, err := EncodeInner(nil, TypeText, StreamChat, payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != 512 {
		t.Fatalf("len %d", len(f))
	}
	in, err := DecodeInner(f)
	if err != nil || in.Type != TypeText || in.Stream != StreamChat || !bytes.Equal(in.Payload, payload) {
		t.Fatalf("%+v %v", in, err)
	}
	if !bytes.Equal(f[308:], make([]byte, 204)) {
		t.Fatal("padding not zero")
	}
	// Append semantics and growth.
	f2, _ := EncodeInner(f, TypePing, StreamControl, AppendPing(nil, 1))
	if len(f2) != 512+256 {
		t.Fatal("append")
	}
	if _, err := DecodeInner(f2[512:]); err != nil {
		t.Fatal(err)
	}
	// Max frame.
	big, err := EncodeInner(nil, TypeImgChunk, 2, make([]byte, MaxPayload))
	if err != nil || len(big) != MaxInner {
		t.Fatal(err)
	}
	if _, err := DecodeInner(big); err != nil {
		t.Fatal(err)
	}
}

func TestInnerRejects(t *testing.T) {
	good, _ := EncodeInner(nil, TypeText, StreamChat, []byte("hi"))
	mut := func(fn func(b []byte) []byte) []byte { c := append([]byte(nil), good...); return fn(c) }
	cases := []struct {
		name string
		b    []byte
		err  error
	}{
		{"short", good[:7], ErrShort},
		{"flags", mut(func(b []byte) []byte { b[1] = 1; return b }), ErrFlags},
		{"padding-extra", append(mut(func(b []byte) []byte { return b }), 0), ErrPadding},
		{"padding-missing", good[:255], ErrPadding},
		{"payload-len-too-big", mut(func(b []byte) []byte { b[4], b[5], b[6], b[7] = 0, 1, 0, 0; return b }), ErrPayloadLength},
		{"payload-len-lies", mut(func(b []byte) []byte { b[7] = 249; return b }), ErrPadding},
		{"unknown-type", mut(func(b []byte) []byte { b[0] = 0x7f; return b }), ErrType},
		{"text-on-control", mut(func(b []byte) []byte { b[3] = 0; return b }), ErrStream},
		{"hello-on-chat", mut(func(b []byte) []byte { b[0] = byte(TypeHello); return b }), ErrStream},
		{"chunk-on-chat", mut(func(b []byte) []byte { b[0] = byte(TypeImgChunk); return b }), ErrStream},
		{"chunk-on-control", mut(func(b []byte) []byte { b[0] = byte(TypeImgChunk); b[3] = 0; return b }), ErrStream},
	}
	for _, c := range cases {
		if _, err := DecodeInner(c.b); !errors.Is(err, c.err) {
			t.Errorf("%s: got %v want %v", c.name, err, c.err)
		}
	}
	if _, err := EncodeInner(nil, TypeText, StreamControl, nil); !errors.Is(err, ErrStream) {
		t.Fatal(err)
	}
	if _, err := EncodeInner(nil, TypeText, StreamChat, make([]byte, MaxPayload+1)); !errors.Is(err, ErrPayloadLength) {
		t.Fatal(err)
	}
	if _, err := EncodeInner(nil, FrameType(0xff), 5, nil); !errors.Is(err, ErrStream) {
		t.Fatal(err)
	}
}

func TestFrameADAndNonce(t *testing.T) {
	ad := FrameAD(make([]byte, 0, ADSize), 7, 9, DirResponderToInitiator)
	want := append([]byte(FrameADPrefix), 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0, 9, 1)
	if !bytes.Equal(ad, want) || len(ad) != ADSize {
		t.Fatalf("%x", ad)
	}
	var n [NonceSize]byte
	FrameNonce(&n, 0x0102030405060708)
	if !bytes.Equal(n[:], []byte{0, 0, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Fatalf("%x", n)
	}
	if a := testing.AllocsPerRun(100, func() { FrameAD(ad, 1, 2, 0); FrameNonce(&n, 3) }); a != 0 {
		t.Fatalf("allocs %v", a)
	}
}

func TestEncodeInnerNoAlloc(t *testing.T) {
	dst := make([]byte, 0, MaxInner)
	payload := make([]byte, 1000)
	if a := testing.AllocsPerRun(100, func() {
		f, _ := EncodeInner(dst, TypeText, StreamChat, payload)
		_, _ = DecodeInner(f)
	}); a != 0 {
		t.Fatalf("allocs %v", a)
	}
}

func FuzzOuterFrame(f *testing.F) {
	f.Add([]byte{0, 0, 1, 16})
	f.Add([]byte{0, 1, 0, 16})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, b []byte) {
		n, err := OuterLen(b)
		if err == nil && (n < MinCiphertext || n > MaxCiphertext) {
			t.Fatalf("accepted %d", n)
		}
		if err == nil {
			var h [4]byte
			if err := PutOuterLen(h[:], n); err != nil || !bytes.Equal(h[:], b[:4]) {
				t.Fatal("round trip")
			}
		}
	})
}

func FuzzInner(f *testing.F) {
	for _, p := range [][]byte{nil, []byte("x"), make([]byte, 248), make([]byte, 249), make([]byte, MaxPayload)} {
		for _, tc := range []struct {
			t FrameType
			s uint16
		}{{TypeHello, 0}, {TypeText, 1}, {TypeImgChunk, 2}} {
			enc, err := EncodeInner(nil, tc.t, tc.s, p)
			if err == nil {
				f.Add(enc)
			}
		}
	}
	f.Add([]byte{1, 1, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		in, err := DecodeInner(b)
		if err != nil {
			return
		}
		if len(b) != PaddedLen(len(in.Payload)) || !streamOK(in.Type, in.Stream) {
			t.Fatal("invariant")
		}
		re, err := EncodeInner(nil, in.Type, in.Stream, in.Payload)
		if err != nil {
			t.Fatal(err)
		}
		// Re-encoding zeros the padding; header+payload must match exactly.
		if !bytes.Equal(re[:InnerHeaderSize+len(in.Payload)], b[:InnerHeaderSize+len(in.Payload)]) || len(re) != len(b) {
			t.Fatal("re-encode mismatch")
		}
	})
}
