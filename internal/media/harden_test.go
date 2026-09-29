package media

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"testing"

	"github.com/guy5116/burrow/internal/wire"
)

// Every prefix of every fixture must strip to an error or a result, never a
// panic; this walks all truncation branches of the four strippers and the
// GIF pre-scan.
func TestTruncationAtEveryOffset(t *testing.T) {
	img := testImage(12, 9)
	frames := []*image.Paletted{palettedFrame(6, 6, color.RGBA{1, 2, 3, 255}), palettedFrame(6, 6, color.RGBA{3, 2, 1, 255})}
	fixtures := map[string][]byte{
		"jpeg": dirtyJPEG(t, img, 1),
		"png":  dirtyPNG(t, img, 1),
		"gif":  dirtyGIF(t, frames),
		"webp": webpFile(4, 4, color.NRGBA{1, 2, 3, 255}, true, 1, false),
	}
	strip := map[string]func([]byte) error{
		"jpeg": func(b []byte) error { _, err := stripJPEG(&bytes.Buffer{}, bytes.NewReader(b)); return err },
		"png":  func(b []byte) error { _, err := stripPNG(&bytes.Buffer{}, bytes.NewReader(b)); return err },
		"gif":  func(b []byte) error { return stripGIF(&bytes.Buffer{}, bytes.NewReader(b)) },
		"webp": func(b []byte) error { _, err := stripWebP(&bytes.Buffer{}, bytes.NewReader(b)); return err },
	}
	for name, data := range fixtures {
		full := len(data) - len(trailing)
		if name == "webp" {
			full = len(data)
		}
		failures := 0
		for n := 0; n < full; n++ {
			if err := strip[name](data[:n]); err != nil {
				failures++
			}
			_, _ = Probe(data[:n])
			if name == "gif" {
				_, _, _ = gifPrescan(data[:n])
			}
		}
		if failures < full/2 {
			t.Errorf("%s: only %d of %d truncations were rejected", name, failures, full)
		}
		if err := strip[name](data); err != nil {
			t.Errorf("%s: full fixture rejected: %v", name, err)
		}
	}
}

// failWriter fails after n bytes so every write-error branch is exercised.
type failWriter struct{ left int }

func (f *failWriter) Write(p []byte) (int, error) {
	if f.left <= 0 {
		return 0, errors.New("disk full")
	}
	if len(p) > f.left {
		n := f.left
		f.left = 0
		return n, errors.New("disk full")
	}
	f.left -= len(p)
	return len(p), nil
}

func TestWriteErrorsPropagate(t *testing.T) {
	img := testImage(200, 150) // large enough to overflow the 64 KiB bufio layers
	big := image.NewNRGBA(image.Rect(0, 0, 400, 400))
	for i := range big.Pix {
		big.Pix[i] = byte(i * 31)
	}
	frames := []*image.Paletted{palettedFrame(300, 300, color.RGBA{1, 2, 3, 255}), palettedFrame(300, 300, color.RGBA{3, 2, 1, 255})}
	for name, run := range map[string]func(w io.Writer) error{
		"jpeg": func(w io.Writer) error { _, err := stripJPEG(w, bytes.NewReader(dirtyJPEG(t, big, 1))); return err },
		"png":  func(w io.Writer) error { _, err := stripPNG(w, bytes.NewReader(dirtyPNG(t, big, 1))); return err },
		"gif":  func(w io.Writer) error { return stripGIF(w, bytes.NewReader(dirtyGIF(t, frames))) },
		"webp": func(w io.Writer) error {
			_, err := stripWebP(w, bytes.NewReader(webpFile(4, 4, color.NRGBA{1, 2, 3, 255}, true, 1, false)))
			return err
		},
	} {
		var full bytes.Buffer
		if err := run(&full); err != nil {
			t.Fatal(name, err)
		}
		for _, budget := range []int{0, 1, 5, 20, 100, 1000, 70000} {
			if budget >= full.Len() {
				continue
			}
			if err := run(&failWriter{left: budget}); err == nil {
				t.Errorf("%s: write failure after %d of %d bytes not reported", name, budget, full.Len())
			}
		}
	}
	_ = img
}

func TestJPEGStructuralCases(t *testing.T) {
	cases := map[string][]byte{
		"no SOI":              []byte("\xff\xe0\x00\x02\xff\xd9"),
		"garbage after SOI":   []byte("\xff\xd8\x00\x01"),
		"short segment len":   []byte("\xff\xd8\xff\xe1\x00\x01"),
		"stray RST":           []byte("\xff\xd8\xff\xd0\xff\xd9"),
		"stray stuffing":      []byte("\xff\xd8\xff\x00\xff\xd9"),
		"truncated payload":   []byte("\xff\xd8\xff\xdb\x00\x10abc"),
		"missing EOI in scan": []byte("\xff\xd8\xff\xda\x00\x02\x01\x02\x03"),
		"scan ends with FF":   []byte("\xff\xd8\xff\xda\x00\x02\x01\xff"),
	}
	for name, in := range cases {
		if _, err := stripJPEG(&bytes.Buffer{}, bytes.NewReader(in)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Fill bytes before markers, RSTn and stuffing inside a scan, a second scan
	// (progressive layout), APP14 kept, and a second JFIF dropped.
	ok := []byte("\xff\xd8" + "\xff\xff\xff\xe0\x00\x07JFIF\x00" + "\xff\xe0\x00\x07JFIF\x00" + "\xff\xee\x00\x04Ad" +
		"\xff\xdb\x00\x03q" + "\xff\xda\x00\x02" + "\x01\xff\x00\x02\xff\xd1\x03\xff\xff" +
		"\xff\xc4\x00\x03h" + "\xff\xda\x00\x02" + "\x09\x08" + "\xff\xd9" + "junk")
	var out bytes.Buffer
	if _, err := stripJPEG(&out, bytes.NewReader(ok)); err != nil {
		t.Fatal(err)
	}
	got := out.Bytes()
	if bytes.Count(got, []byte("JFIF")) != 1 || !bytes.Contains(got, []byte("\xff\xee\x00\x04Ad")) ||
		!bytes.Contains(got, []byte("\x01\xff\x00\x02\xff\xd1\x03")) || bytes.Count(got, []byte("\xff\xda")) != 2 ||
		!bytes.HasSuffix(got, []byte("\xff\xd9")) {
		t.Fatalf("%q", got)
	}
}

func TestPNGAndWebPStructuralCases(t *testing.T) {
	png1 := dirtyPNG(t, testImage(4, 4), 1)
	notIHDR := append(append([]byte(nil), pngSig...), pngChunk("tEXt", []byte("x"))...)
	huge := append(append([]byte(nil), pngSig...), 0xff, 0xff, 0xff, 0xff, 'I', 'H', 'D', 'R')
	for name, in := range map[string][]byte{"bad sig": []byte("\x89PNX\r\n\x1a\n"), "not IHDR first": notIHDR, "huge chunk": huge, "no IEND": png1[:len(png1)-len(trailing)-12]} {
		if _, err := stripPNG(&bytes.Buffer{}, bytes.NewReader(in)); !errors.Is(err, ErrCorrupt) {
			t.Errorf("png %s: %v", name, err)
		}
	}
	w := webpFile(4, 4, color.NRGBA{1, 2, 3, 255}, true, 1, false)
	short := append([]byte(nil), w...)
	short[4], short[5], short[6], short[7] = 2, 0, 0, 0 // RIFF size < 4
	over := append([]byte(nil), w...)
	over[17] = 0xff // first chunk claims more than the RIFF holds
	tinyVP8X := []byte("RIFF\x10\x00\x00\x00WEBPVP8X\x04\x00\x00\x00\x00\x00\x00\x00")
	anmf := append([]byte("RIFF\x14\x00\x00\x00WEBP"), riffChunk("ANMF", []byte("12345678"))...)
	for name, c := range map[string]struct {
		in  []byte
		err error
	}{"riff too small": {short, ErrCorrupt}, "chunk overflow": {over, ErrCorrupt}, "short VP8X": {tinyVP8X, ErrCorrupt},
		"not riff": {[]byte("RIFX\x00\x00\x00\x00WEBP"), ErrCorrupt}, "ANMF": {anmf, ErrAnimated}} {
		if _, err := stripWebP(&bytes.Buffer{}, bytes.NewReader(c.in)); !errors.Is(err, c.err) {
			t.Errorf("webp %s: %v", name, err)
		}
	}
	// A bare EXIF chunk without the "Exif\0\0" prefix still yields the orientation; odd-sized chunks are padded.
	body := append(riffChunk("VP8L", vp8l(2, 2, color.NRGBA{9, 9, 9, 255})), riffChunk("EXIF", tiffWithOrientation(3))...)
	body = append(body, riffChunk("XMP ", []byte("odd"))...)
	file := append([]byte("RIFF\x00\x00\x00\x00WEBP"), body...)
	file[4] = byte(4 + len(body))
	o, err := stripWebP(&bytes.Buffer{}, bytes.NewReader(file))
	if err != nil || o != 3 {
		t.Fatal(o, err)
	}
	if webpAnimated([]byte("short")) || !webpAnimated(append([]byte("RIFF\x00\x00\x00\x00WEBPVP8L"), []byte("....ANIM")...)) {
		t.Fatal("webpAnimated")
	}
}

func TestGIFStructuralCases(t *testing.T) {
	// Local color tables, GIF87a header, and unknown block types.
	p := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})
	g := dirtyGIF(t, []*image.Paletted{p})
	hdrLen := 13
	if g[10]&0x80 != 0 {
		hdrLen += 3 << (g[10]&7 + 1)
	}
	lct := append([]byte(nil), g[:hdrLen]...)
	lct = append(lct, 0x2C, 0, 0, 0, 0, 2, 0, 2, 0, 0x80) // image descriptor with a 2-entry local table
	lct = append(lct, 1, 2, 3, 4, 5, 6)                   // LCT
	lct = append(lct, 2, 2, 0x4c, 0x01, 0, 0x3B)          // LZW min size, one sub-block, terminator, trailer
	var out bytes.Buffer
	if err := stripGIF(&out, bytes.NewReader(lct)); err != nil || !bytes.Contains(out.Bytes(), []byte{1, 2, 3, 4, 5, 6}) {
		t.Fatal(err)
	}
	if f, a, err := gifPrescan(lct); err != nil || f != 1 || a != 4 {
		t.Fatal(f, a, err)
	}
	g87 := append([]byte("GIF87a"), lct[6:]...)
	if err := stripGIF(&bytes.Buffer{}, bytes.NewReader(g87)); err != nil {
		t.Fatal(err)
	}
	bad := append(append([]byte(nil), g[:hdrLen]...), 0x99)
	if err := stripGIF(&bytes.Buffer{}, bytes.NewReader(bad)); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, _, err := gifPrescan(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, _, err := gifPrescan([]byte("GIF89b.......")); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, err := DecodeGIF(dirtyPNG(t, testImage(2, 2), 1)); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := DecodeGIF([]byte("GIF89a")); err == nil {
		t.Fatal("truncated gif decoded")
	}
}

func TestSmallHelpers(t *testing.T) {
	for f, ext := range map[Format]string{wire.FormatPNG: "png", wire.FormatJPEG: "jpg", wire.FormatWebP: "webp", wire.FormatGIF: "gif", 0: "bin"} {
		if Ext(f) != ext {
			t.Errorf("Ext(%d)", f)
		}
	}
	for m, want := range map[color.Model]int{color.GrayModel: 1, color.AlphaModel: 1, color.Gray16Model: 2, color.YCbCrModel: 3,
		color.RGBA64Model: 8, color.NRGBA64Model: 8, color.RGBAModel: 4, color.CMYKModel: 4} {
		if got := bytesPerPixel(m); got != want {
			t.Errorf("bpp %v = %d", m, got)
		}
	}
	if bytesPerPixel(color.Palette{color.Black}) != 1 {
		t.Fatal("palette bpp")
	}
	if _, err := readAll(bytes.NewReader(make([]byte, 11)), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if b, err := readAll(bytes.NewReader(make([]byte, 10)), 10); err != nil || len(b) != 10 {
		t.Fatal(err)
	}
	h := NewHasher()
	h.Write([]byte("x"))
	if len(h.Sum(nil)) != wire.HashSize {
		t.Fatal("hasher")
	}
	if ErrDiverged.Error() == "" {
		t.Fatal("error text")
	}
	if th := Thumbnail(testImage(2, 1000), 100); th.Bounds().Dy() != 100 || th.Bounds().Dx() != 1 {
		t.Fatal(th.Bounds())
	}
	if applyOrientation(testImage(2, 2), 9).Bounds().Dx() != 2 {
		t.Fatal("orientation 9 must be a no-op")
	}
	// Decode of corrupt pixel data after a valid header.
	bad := dirtyPNG(t, testImage(8, 8), 1)
	bad = bad[:len(bad)-len(trailing)-30]
	if _, _, err := Decode(bad); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, _, err := Decode([]byte("nope")); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	// Static GIF decodes through Decode; static GIF in paranoid mode becomes PNG.
	sg := dirtyGIF(t, []*image.Paletted{palettedFrame(5, 5, color.RGBA{7, 7, 7, 255})})
	if _, info, err := Decode(sg); err != nil || info.Format != wire.FormatGIF || info.Animated {
		t.Fatal(err)
	}
	p, _ := prepareAndStream(t, writeTemp(t, "s.gif", sg), ModeParanoid)
	if p.Format != wire.FormatPNG {
		t.Fatal("static gif paranoid")
	}
}

func TestPrepareAndStreamErrors(t *testing.T) {
	if _, err := Prepare("/definitely/missing.png", ModeStrip, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := ProbeFile(t.TempDir()); err == nil {
		t.Fatal("directory probed")
	}
	path := writeTemp(t, "a.png", dirtyPNG(t, testImage(300, 300), 1))
	p, err := Prepare(path, ModeStrip, 0)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("sink failed")
	if err := Stream(p, func(uint32, []byte) error { return boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Stream(p, func(uint32, []byte) error { return nil }); err == nil {
		t.Fatal("stream of a missing file succeeded")
	}
	// Replacing the file with another format is caught by the extension check.
	if err := os.WriteFile(path, dirtyJPEG(t, testImage(300, 300), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Stream(p, func(uint32, []byte) error { return nil }); !errors.Is(err, ErrMismatch) {
		t.Fatal(err)
	}
	// Re-encode paths surface decode failures.
	bad := dirtyJPEG(t, testImage(16, 16), 6)
	bad = bad[:len(bad)-len(trailing)-40]
	bad = append(bad, 0xff, 0xd9)
	if _, err := Prepare(writeTemp(t, "bad.jpg", bad), ModeStrip, 0); err == nil {
		t.Fatal("corrupt rotated jpeg accepted")
	}
}

func TestExifParserEdges(t *testing.T) {
	le := tiffWithOrientation(6)
	for name, mod := range map[string]func([]byte){
		"bad magic":        func(b []byte) { b[2] = 43 },
		"ifd offset small": func(b []byte) { b[4] = 4 },
		"too many entries": func(b []byte) { b[8], b[9] = 0xff, 0xff },
		"wrong type":       func(b []byte) { b[12] = 4 },
		"wrong count":      func(b []byte) { b[14] = 2 },
	} {
		c := append([]byte(nil), le...)
		mod(c)
		if exifOrientation(c) != 0 {
			t.Errorf("%s accepted", name)
		}
	}
	if exifOrientation(le[:15]) != 0 {
		t.Fatal("truncated entry accepted")
	}
	// Orientation absent: other tags only.
	none := []byte("II\x2a\x00\x08\x00\x00\x00\x01\x00")
	none = append(none, entry(0x010F, 2, 1, 0)...)
	if exifOrientation(none) != 0 {
		t.Fatal("phantom orientation")
	}
}
