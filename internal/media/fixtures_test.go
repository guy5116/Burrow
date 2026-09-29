package media

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// testImage is a small gradient with an asymmetric mark so rotations are detectable.
func testImage(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	img.Set(0, 0, color.NRGBA{255, 255, 255, 255})
	return img
}

// tiffWithOrientation builds a little-endian TIFF blob with Orientation and a
// GPS IFD pointer (the metadata we must never leak).
func tiffWithOrientation(o uint16) []byte {
	b := []byte("II\x2a\x00\x08\x00\x00\x00")
	entries := [][]byte{
		entry(0x0112, 3, 1, uint32(o)),
		entry(0x8825, 4, 1, 60), // GPS IFD at offset 60
	}
	b = append(b, byte(len(entries)), 0)
	for _, e := range entries {
		b = append(b, e...)
	}
	b = append(b, 0, 0, 0, 0)
	for len(b) < 60 {
		b = append(b, 0)
	}
	b = append(b, 1, 0)
	b = append(b, entry(0x0002, 5, 3, 80)...) // GPSLatitude → rationals at 80
	b = append(b, 0, 0, 0, 0)
	for len(b) < 80 {
		b = append(b, 0)
	}
	for i := 0; i < 3; i++ {
		b = binary.LittleEndian.AppendUint32(b, uint32(48+i))
		b = binary.LittleEndian.AppendUint32(b, 1)
	}
	return b
}

func entry(tag, typ uint16, count, val uint32) []byte {
	e := binary.LittleEndian.AppendUint16(nil, tag)
	e = binary.LittleEndian.AppendUint16(e, typ)
	e = binary.LittleEndian.AppendUint32(e, count)
	if typ == 3 && count == 1 {
		e = binary.LittleEndian.AppendUint16(e, uint16(val))
		e = append(e, 0, 0)
	} else {
		e = binary.LittleEndian.AppendUint32(e, val)
	}
	return e
}

var testColor = color.NRGBA{R: 10, G: 200, B: 30, A: 255}

// Secrets that must not survive stripping.
var (
	secretComment = []byte("secret comment about the sender")
	secretXMP     = []byte("<x:xmpmeta xmlns:x=\"adobe:ns:meta/\">creator=Alice</x:xmpmeta>")
	secretICC     = []byte("ICC_PROFILE\x00fakeprofilebytes")
	secretIPTC    = []byte("Photoshop 3.0\x008BIMcaptionbyline")
	trailing      = []byte("TRAILING GARBAGE WITH GPS 48.85")
)

func jpegSegment(marker byte, payload []byte) []byte {
	out := []byte{0xFF, marker}
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2))
	return append(out, payload...)
}

// dirtyJPEG encodes img and injects EXIF (with orientation o), XMP, ICC,
// IPTC, a COM segment and trailing bytes.
func dirtyJPEG(t testing.TB, img image.Image, o uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes() // Go's encoder emits no APP0; add a fat JFIF with a thumbnail so the rewrite is exercised
	out := append([]byte(nil), clean[:2]...)
	fatJFIF := append([]byte("JFIF\x00\x01\x02\x01\x00\x48\x00\x48\x04\x04"), bytes.Repeat([]byte("T"), 48)...)
	out = append(out, jpegSegment(0xE0, fatJFIF)...)
	out = append(out, jpegSegment(0xE1, append([]byte("Exif\x00\x00"), tiffWithOrientation(o)...))...)
	out = append(out, jpegSegment(0xE1, append([]byte("http://ns.adobe.com/xap/1.0/\x00"), secretXMP...))...)
	out = append(out, jpegSegment(0xE2, secretICC)...)
	out = append(out, jpegSegment(0xED, secretIPTC)...)
	out = append(out, jpegSegment(0xFE, secretComment)...)
	out = append(out, clean[2:]...)
	return append(out, trailing...)
}

func pngChunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	return binary.BigEndian.AppendUint32(out, crc.Sum32())
}

// dirtyPNG encodes img and injects tEXt, iTXt, tIME, pHYs, iCCP, eXIf and trailing bytes.
func dirtyPNG(t testing.TB, img image.Image, o uint16) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()
	ihdrEnd := 8 + 8 + 13 + 4
	out := append([]byte(nil), clean[:ihdrEnd]...)
	out = append(out, pngChunk("tEXt", append([]byte("Comment\x00"), secretComment...))...)
	out = append(out, pngChunk("iTXt", append([]byte("XML:com.adobe.xmp\x00\x00\x00\x00\x00"), secretXMP...))...)
	out = append(out, pngChunk("tIME", []byte{7, 0xE8, 1, 2, 3, 4, 5})...)
	out = append(out, pngChunk("pHYs", []byte{0, 0, 0x0B, 0x13, 0, 0, 0x0B, 0x13, 1})...)
	out = append(out, pngChunk("iCCP", secretICC)...)
	out = append(out, pngChunk("eXIf", tiffWithOrientation(o))...)
	out = append(out, clean[ihdrEnd:]...)
	return append(out, trailing...)
}

func gifSubBlocks(data []byte) []byte {
	var out []byte
	for len(data) > 0 {
		n := min(255, len(data))
		out = append(out, byte(n))
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, 0)
}

// dirtyGIF encodes frames (animated when > 1) and injects a comment, a plain
// text extension, an XMP application extension and trailing bytes; the
// NETSCAPE loop block (from the encoder) stays.
func dirtyGIF(t testing.TB, frames []*image.Paletted) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0}
	for _, f := range frames {
		g.Image = append(g.Image, f)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()
	hdrLen := 13
	if clean[10]&0x80 != 0 {
		hdrLen += 3 << (clean[10]&7 + 1)
	}
	out := append([]byte(nil), clean[:hdrLen]...)
	out = append(out, 0x21, 0xFE)
	out = append(out, gifSubBlocks(secretComment)...)
	out = append(out, 0x21, 0xFF)
	out = append(out, gifSubBlocks(append([]byte("XMP DataXMP"), secretXMP...))...)
	out = append(out, 0x21, 0x01, 12, 0, 0, 0, 0, 10, 0, 10, 0, 8, 8, 1, 0)
	out = append(out, gifSubBlocks([]byte("plain text secret"))...)
	out = append(out, clean[hdrLen:]...)
	return append(out, trailing...)
}

func palettedFrame(w, h int, c color.Color) *image.Paletted {
	p := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, c, color.White})
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p.SetColorIndex(x, y, 1)
		}
	}
	p.SetColorIndex(0, 0, 2)
	return p
}

// --- WebP: a hand-assembled VP8L lossless bitstream (Go has no encoder) ---

type bitWriter struct {
	buf  []byte
	acc  uint64
	nacc uint
}

func (b *bitWriter) write(v uint64, n uint) {
	b.acc |= v << b.nacc
	b.nacc += n
	for b.nacc >= 8 {
		b.buf = append(b.buf, byte(b.acc))
		b.acc >>= 8
		b.nacc -= 8
	}
}

func (b *bitWriter) bytes() []byte {
	if b.nacc > 0 {
		b.buf = append(b.buf, byte(b.acc))
	}
	return b.buf
}

// vp8l builds a w×h solid-color lossless image: five single-symbol prefix
// codes mean each pixel costs zero bits.
func vp8l(w, h int, c color.NRGBA) []byte {
	bw := &bitWriter{}
	bw.write(0x2f, 8)
	bw.write(uint64(w-1), 14)
	bw.write(uint64(h-1), 14)
	bw.write(1, 1) // alpha used
	bw.write(0, 3) // version
	bw.write(0, 1) // no transform
	bw.write(0, 1) // no color cache
	bw.write(0, 1) // no meta prefix codes
	for _, sym := range []uint64{uint64(c.G), uint64(c.R), uint64(c.B), uint64(c.A), 0} {
		bw.write(1, 1) // simple code
		bw.write(0, 1) // one symbol
		bw.write(1, 1) // 8-bit symbol
		bw.write(sym, 8)
	}
	return bw.bytes()
}

func riffChunk(fourcc string, data []byte) []byte {
	out := append([]byte(fourcc), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(out[4:], uint32(len(data)))
	out = append(out, data...)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// webpFile wraps a VP8L bitstream in a RIFF container. With extended=true it
// adds VP8X with ICC/EXIF/XMP flags and the corresponding chunks.
func webpFile(w, h int, c color.NRGBA, extended bool, orientation uint16, animated bool) []byte {
	var body []byte
	if extended {
		flags := byte(vp8xICC | vp8xEXIF | vp8xXMP | 0x10) // + alpha
		if animated {
			flags |= vp8xAnim
		}
		v := []byte{flags, 0, 0, 0}
		v = append(v, byte(w-1), byte((w-1)>>8), byte((w-1)>>16), byte(h-1), byte((h-1)>>8), byte((h-1)>>16))
		body = append(body, riffChunk("VP8X", v)...)
		body = append(body, riffChunk("ICCP", secretICC)...)
	}
	body = append(body, riffChunk("VP8L", vp8l(w, h, c))...)
	if extended {
		body = append(body, riffChunk("EXIF", append([]byte("Exif\x00\x00"), tiffWithOrientation(orientation)...))...)
		body = append(body, riffChunk("XMP ", secretXMP)...)
		if animated {
			body = append(body, riffChunk("ANIM", []byte{0, 0, 0, 0, 0, 0})...)
		}
	}
	out := []byte("RIFF\x00\x00\x00\x00WEBP")
	binary.LittleEndian.PutUint32(out[4:], uint32(4+len(body)))
	return append(out, body...)
}
