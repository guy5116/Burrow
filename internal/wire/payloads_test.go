package wire

import (
	"bytes"
	"errors"
	"testing"
)

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestHello(t *testing.T) {
	h := Hello{Features: FeatureImages | FeatureTyping, MaxImage: 1 << 20, Name: []byte("Zoë")}
	p, err := AppendHello(nil, h)
	mustOK(t, err)
	got, err := DecodeHello(p)
	mustOK(t, err)
	if got.Features != h.Features || got.MaxImage != h.MaxImage || !bytes.Equal(got.Name, h.Name) {
		t.Fatalf("%+v", got)
	}
	// default HELLO: no features, no image, empty name
	p, _ = AppendHello(nil, Hello{})
	if _, err := DecodeHello(p); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendHello(nil, Hello{Name: make([]byte, 33)}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	bad := []struct {
		name string
		h    Hello
		err  error
	}{
		{"reserved-bit", Hello{Features: 1 << 3}, ErrValue},
		{"high-bit", Hello{Features: 1 << 63}, ErrValue},
		{"images-no-max", Hello{Features: FeatureImages}, ErrValue},
		{"max-no-images", Hello{MaxImage: 5}, ErrValue},
	}
	for _, c := range bad {
		p, _ := AppendHello(nil, c.h)
		if _, err := DecodeHello(p); !errors.Is(err, c.err) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	p, _ = AppendHello(nil, Hello{Name: []byte{0xff, 0xfe}})
	if _, err := DecodeHello(p); !errors.Is(err, ErrUTF8) {
		t.Fatal(err)
	}
	p, _ = AppendHello(nil, Hello{Name: []byte("ab")})
	p[16] = 1 // name_len lies
	if _, err := DecodeHello(p); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	p[16] = 40
	if _, err := DecodeHello(p); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	if _, err := DecodeHello(p[:10]); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	long, _ := AppendHello(nil, Hello{Name: make([]byte, 32)})
	long[16] = 33
	long = append(long, 0)
	if _, err := DecodeHello(long); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestSmallPayloads(t *testing.T) {
	if n, err := DecodePing(AppendPing(nil, 42)); err != nil || n != 42 {
		t.Fatal(n, err)
	}
	if n, err := DecodePong(AppendPong(nil, 42)); err != nil || n != 42 {
		t.Fatal(n, err)
	}
	if _, err := DecodePing(make([]byte, 7)); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if _, err := DecodePing(make([]byte, 9)); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	for r := uint8(0); r <= ByeResourceLimit; r++ {
		if got, err := DecodeBye(AppendBye(nil, r)); err != nil || got != r {
			t.Fatal(r, err)
		}
	}
	if _, err := DecodeBye([]byte{5}); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if _, err := DecodeBye(nil); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if _, err := DecodeTyping([]byte{2}); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if s, err := DecodeTyping(AppendTyping(nil, TypingStart)); err != nil || s != 1 {
		t.Fatal(err)
	}
	if _, err := DecodeImgReject([]byte{4}); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if r, err := DecodeImgReject(AppendImgReject(nil, RejectBusy)); err != nil || r != 3 {
		t.Fatal(err)
	}
	if _, err := DecodeImgResult([]byte{3}); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if r, err := DecodeImgResult(AppendImgResult(nil, ResultHashMismatch)); err != nil || r != 1 {
		t.Fatal(err)
	}
	if _, err := DecodeImgCancel([]byte{4}); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if r, err := DecodeImgCancel(AppendImgCancel(nil, CancelShutdown)); err != nil || r != 3 {
		t.Fatal(err)
	}
	if n, err := DecodeImgAccept(AppendImgAccept(nil, 9)); err != nil || n != 9 {
		t.Fatal(err)
	}
	if _, err := DecodeImgAccept([]byte{1, 2, 3}); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if err := DecodeEmpty(nil); err != nil {
		t.Fatal(err)
	}
	if err := DecodeEmpty([]byte{0}); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
}

func TestText(t *testing.T) {
	in := Text{MsgID: 1 << 63, SentAt: -5, Text: []byte("héllo\n")}
	p, err := AppendText(nil, in)
	mustOK(t, err)
	got, err := DecodeText(p)
	mustOK(t, err)
	if got.MsgID != in.MsgID || got.SentAt != in.SentAt || !bytes.Equal(got.Text, in.Text) {
		t.Fatalf("%+v", got)
	}
	if _, err := DecodeText(p[:15]); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if _, err := DecodeText(append(p[:16:16], 0xc0)); !errors.Is(err, ErrUTF8) {
		t.Fatal(err)
	}
	if _, err := AppendText(nil, Text{Text: make([]byte, MaxTextBytes+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := DecodeText(make([]byte, 16+MaxTextBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := DecodeText(make([]byte, 16+MaxTextBytes)); err != nil {
		t.Fatal(err)
	}
	// empty text is legal at the wire level
	if _, err := DecodeText(p[:16]); err != nil {
		t.Fatal(err)
	}
}

func TestAck(t *testing.T) {
	a := Ack{MsgID: 77, Status: AckDelivered}
	got, err := DecodeAck(AppendAck(nil, a))
	if err != nil || got != a {
		t.Fatal(got, err)
	}
	if _, err := DecodeAck(AppendAck(nil, Ack{MsgID: 1, Status: 2})); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if _, err := DecodeAck(make([]byte, 8)); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if _, err := DecodeAck(make([]byte, 10)); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
}

func TestImgOffer(t *testing.T) {
	o := ImgOffer{Size: 12345, Format: FormatWebP, Width: 640, Height: 480, Caption: []byte("cat")}
	for i := range o.Hash {
		o.Hash[i] = byte(i)
	}
	p, err := AppendImgOffer(nil, o)
	mustOK(t, err)
	got, err := DecodeImgOffer(p)
	mustOK(t, err)
	if got.Size != o.Size || got.Format != o.Format || got.Width != o.Width || got.Height != o.Height || got.Hash != o.Hash || !bytes.Equal(got.Caption, o.Caption) {
		t.Fatalf("%+v", got)
	}
	if _, err := AppendImgOffer(nil, ImgOffer{Caption: make([]byte, MaxCaptionBytes+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	try := func(mod func(*ImgOffer), want error) {
		t.Helper()
		x := o
		mod(&x)
		p, _ := AppendImgOffer(nil, x)
		if _, err := DecodeImgOffer(p); !errors.Is(err, want) {
			t.Errorf("got %v want %v", err, want)
		}
	}
	try(func(x *ImgOffer) { x.Size = 0 }, ErrValue)
	try(func(x *ImgOffer) { x.Format = 0 }, ErrValue)
	try(func(x *ImgOffer) { x.Format = 5 }, ErrValue)
	try(func(x *ImgOffer) { x.Caption = []byte{0xff} }, ErrUTF8)
	try(func(x *ImgOffer) { x.Caption = nil }, nil)
	if _, err := DecodeImgOffer(p[:imgOfferFixed-1]); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	q := append([]byte(nil), p...)
	q[imgOfferFixed-1] = 2 // caption_len lies
	if _, err := DecodeImgOffer(q); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	q = append(q[:imgOfferFixed], make([]byte, MaxCaptionBytes+1)...)
	q[imgOfferFixed-2], q[imgOfferFixed-1] = 4, 1 // 1025
	if _, err := DecodeImgOffer(q); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestImgChunk(t *testing.T) {
	c := ImgChunk{Index: 3, Data: bytes.Repeat([]byte{1}, ChunkData)}
	p, err := AppendImgChunk(nil, c)
	mustOK(t, err)
	if len(p) != MaxPayload {
		t.Fatal(len(p))
	}
	got, err := DecodeImgChunk(p)
	if err != nil || got.Index != 3 || !bytes.Equal(got.Data, c.Data) {
		t.Fatal(err)
	}
	if _, err := AppendImgChunk(nil, ImgChunk{}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := AppendImgChunk(nil, ImgChunk{Data: make([]byte, ChunkData+1)}); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := DecodeImgChunk(p[:4]); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if _, err := DecodeImgChunk(append(p, 0)); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestRekey(t *testing.T) {
	var ri RekeyInit
	ri.EPub[0], ri.KemEK[1183] = 1, 2
	p := AppendRekeyInit(nil, &ri)
	var got RekeyInit
	mustOK(t, DecodeRekeyInit(p, &got))
	if got != ri {
		t.Fatal("init")
	}
	if err := DecodeRekeyInit(p[:len(p)-1], &got); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if err := DecodeRekeyInit(append(p, 0), &got); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	var rr RekeyResp
	rr.EPub[31], rr.KemCT[0] = 3, 4
	p = AppendRekeyResp(nil, &rr)
	var gotR RekeyResp
	mustOK(t, DecodeRekeyResp(p, &gotR))
	if gotR != rr {
		t.Fatal("resp")
	}
	if err := DecodeRekeyResp(p[:5], &gotR); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
}

func TestHandshakePayloads(t *testing.T) {
	var h1 HS1
	h1.KemEK[0] = 9
	var g1 HS1
	mustOK(t, DecodeHS1(AppendHS1(nil, &h1), &g1))
	if g1 != h1 {
		t.Fatal("hs1")
	}
	if err := DecodeHS1(nil, &g1); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	var h2 HS2
	h2.KemCT[5], h2.R[31] = 1, 2
	var g2 HS2
	mustOK(t, DecodeHS2(AppendHS2(nil, &h2), &g2))
	if g2 != h2 {
		t.Fatal("hs2")
	}
	if err := DecodeHS2(make([]byte, HS2PayloadLen+1), &g2); !errors.Is(err, ErrTrailing) {
		t.Fatal(err)
	}
	h3 := HS3{HasToken: true}
	h3.R[0], h3.Token[15] = 7, 8
	p := AppendHS3(nil, &h3)
	var g3 HS3
	mustOK(t, DecodeHS3(p, &g3))
	if g3 != h3 {
		t.Fatal("hs3")
	}
	// no token: token bytes must be zero on the wire even if set in the struct
	h3.HasToken = false
	p = AppendHS3(nil, &h3)
	if !bytes.Equal(p[33:], make([]byte, 16)) {
		t.Fatal("token leaked")
	}
	mustOK(t, DecodeHS3(p, &g3))
	if g3.HasToken || g3.Token != [16]byte{} {
		t.Fatal("hs3 no token")
	}
	p[40] = 1 // non-zero token with has_token=0
	if err := DecodeHS3(p, &g3); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	p[32] = 2
	if err := DecodeHS3(p, &g3); !errors.Is(err, ErrValue) {
		t.Fatal(err)
	}
	if err := DecodeHS3(p[:10], &g3); !errors.Is(err, ErrShort) {
		t.Fatal(err)
	}
	if HSMessageLen(1) != HS1Len || HSMessageLen(2) != HS2Len || HSMessageLen(3) != HS3Len || HSMessageLen(4) != 0 {
		t.Fatal("HSMessageLen")
	}
}

// Fuzzers: one per payload type. Each checks that a successful decode
// re-encodes to identical bytes (byte-exactness) and never panics.

func FuzzHelloDecode(f *testing.F) {
	p, _ := AppendHello(nil, Hello{Features: FeatureImages, MaxImage: 1, Name: []byte("n")})
	f.Add(p)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		h, err := DecodeHello(b)
		if err != nil {
			return
		}
		re, err := AppendHello(nil, h)
		if err != nil || !bytes.Equal(re, b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzPingDecode(f *testing.F) {
	f.Add(AppendPing(nil, 1))
	f.Fuzz(func(t *testing.T, b []byte) {
		if n, err := DecodePing(b); err == nil && !bytes.Equal(AppendPing(nil, n), b) {
			t.Fatal("re-encode")
		}
		if n, err := DecodePong(b); err == nil && !bytes.Equal(AppendPong(nil, n), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzByeDecode(f *testing.F) {
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := DecodeBye(b); err == nil && !bytes.Equal(AppendBye(nil, r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzTextDecode(f *testing.F) {
	p, _ := AppendText(nil, Text{MsgID: 1, SentAt: 2, Text: []byte("x")})
	f.Add(p)
	f.Fuzz(func(t *testing.T, b []byte) {
		x, err := DecodeText(b)
		if err != nil {
			return
		}
		if re, err := AppendText(nil, x); err != nil || !bytes.Equal(re, b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzAckDecode(f *testing.F) {
	f.Add(AppendAck(nil, Ack{MsgID: 1, Status: 1}))
	f.Fuzz(func(t *testing.T, b []byte) {
		if a, err := DecodeAck(b); err == nil && !bytes.Equal(AppendAck(nil, a), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzTypingDecode(f *testing.F) {
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, b []byte) {
		if s, err := DecodeTyping(b); err == nil && !bytes.Equal(AppendTyping(nil, s), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgOfferDecode(f *testing.F) {
	p, _ := AppendImgOffer(nil, ImgOffer{Size: 1, Format: 1, Caption: []byte("c")})
	f.Add(p)
	f.Fuzz(func(t *testing.T, b []byte) {
		o, err := DecodeImgOffer(b)
		if err != nil {
			return
		}
		if re, err := AppendImgOffer(nil, o); err != nil || !bytes.Equal(re, b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgAcceptDecode(f *testing.F) {
	f.Add(AppendImgAccept(nil, 0))
	f.Fuzz(func(t *testing.T, b []byte) {
		if n, err := DecodeImgAccept(b); err == nil && !bytes.Equal(AppendImgAccept(nil, n), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgRejectDecode(f *testing.F) {
	f.Add([]byte{3})
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := DecodeImgReject(b); err == nil && !bytes.Equal(AppendImgReject(nil, r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgChunkDecode(f *testing.F) {
	p, _ := AppendImgChunk(nil, ImgChunk{Index: 1, Data: []byte{1}})
	f.Add(p)
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := DecodeImgChunk(b)
		if err != nil {
			return
		}
		if re, err := AppendImgChunk(nil, c); err != nil || !bytes.Equal(re, b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgDoneDecode(f *testing.F) {
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		if err := DecodeEmpty(b); (err == nil) != (len(b) == 0) {
			t.Fatal("empty")
		}
	})
}

func FuzzImgResultDecode(f *testing.F) {
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := DecodeImgResult(b); err == nil && !bytes.Equal(AppendImgResult(nil, r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzImgCancelDecode(f *testing.F) {
	f.Add([]byte{0})
	f.Fuzz(func(t *testing.T, b []byte) {
		if r, err := DecodeImgCancel(b); err == nil && !bytes.Equal(AppendImgCancel(nil, r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzRekeyInitDecode(f *testing.F) {
	f.Add(AppendRekeyInit(nil, &RekeyInit{}))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r RekeyInit
		if err := DecodeRekeyInit(b, &r); err == nil && !bytes.Equal(AppendRekeyInit(nil, &r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzRekeyRespDecode(f *testing.F) {
	f.Add(AppendRekeyResp(nil, &RekeyResp{}))
	f.Fuzz(func(t *testing.T, b []byte) {
		var r RekeyResp
		if err := DecodeRekeyResp(b, &r); err == nil && !bytes.Equal(AppendRekeyResp(nil, &r), b) {
			t.Fatal("re-encode")
		}
	})
}

func FuzzHandshakePayloads(f *testing.F) {
	f.Add(1, AppendHS1(nil, &HS1{}))
	f.Add(2, AppendHS2(nil, &HS2{}))
	f.Add(3, AppendHS3(nil, &HS3{}))
	f.Add(3, AppendHS3(nil, &HS3{HasToken: true, Token: [16]byte{1}}))
	f.Fuzz(func(t *testing.T, which int, b []byte) {
		switch which {
		case 1:
			var h HS1
			if err := DecodeHS1(b, &h); err == nil && !bytes.Equal(AppendHS1(nil, &h), b) {
				t.Fatal("hs1")
			}
		case 2:
			var h HS2
			if err := DecodeHS2(b, &h); err == nil && !bytes.Equal(AppendHS2(nil, &h), b) {
				t.Fatal("hs2")
			}
		default:
			var h HS3
			if err := DecodeHS3(b, &h); err == nil && !bytes.Equal(AppendHS3(nil, &h), b) {
				t.Fatal("hs3")
			}
		}
	})
}
