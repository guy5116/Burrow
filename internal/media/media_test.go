package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/guy5116/burrow/internal/wire"
)

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertNoSecrets(t *testing.T, out []byte) {
	t.Helper()
	for _, s := range [][]byte{secretComment, secretXMP, secretICC, secretIPTC, trailing, []byte("Exif"), []byte("GPS"), []byte("xmp"), []byte("ICC_PROFILE"), []byte("plain text secret")} {
		if bytes.Contains(out, s) {
			t.Fatalf("stripped output still contains %q", s)
		}
	}
}

func samePixels(t *testing.T, a, b image.Image) {
	t.Helper()
	if a.Bounds() != b.Bounds() {
		t.Fatalf("bounds %v vs %v", a.Bounds(), b.Bounds())
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			if color.NRGBAModel.Convert(a.At(x, y)) != color.NRGBAModel.Convert(b.At(x, y)) {
				t.Fatalf("pixel (%d,%d) differs", x, y)
			}
		}
	}
}

// prepareAndStream runs both passes and returns the streamed bytes.
func prepareAndStream(t *testing.T, path string, mode Mode) (Prepared, []byte) {
	t.Helper()
	p, err := Prepare(context.Background(), path, mode, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	var next uint32
	if err := Stream(context.Background(), p, func(i uint32, chunk []byte) error {
		if i != next {
			t.Fatalf("chunk index %d want %d", i, next)
		}
		next++
		out = append(out, chunk...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if uint64(len(out)) != p.Size {
		t.Fatalf("size %d want %d", len(out), p.Size)
	}
	return p, out
}

func TestSniff(t *testing.T) {
	cases := map[string]Format{
		"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR": wire.FormatPNG,
		"\xff\xd8\xff\xe0\x00\x10JFIF":        wire.FormatJPEG,
		"RIFF\x00\x00\x00\x00WEBPVP8L":        wire.FormatWebP,
		"GIF89a\x01\x00\x01\x00":              wire.FormatGIF,
		"GIF87a":                              wire.FormatGIF,
	}
	for in, want := range cases {
		if got, err := Sniff([]byte(in)); err != nil || got != want {
			t.Errorf("Sniff(%q)=%d,%v", in, got, err)
		}
	}
	for _, in := range []string{"", "GIF", "BM6", "\xff\xd8\x00", "RIFF\x00\x00\x00\x00WAVE"} {
		if _, err := Sniff([]byte(in)); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Sniff(%q) accepted", in)
		}
	}
	if Ext(wire.FormatJPEG) != "jpg" || Ext(9) != "bin" {
		t.Fatal("ext")
	}
}

func TestJPEGStrip(t *testing.T) {
	img := testImage(40, 30)
	path := writeTemp(t, "a.jpg", dirtyJPEG(t, img, 1))
	p, out := prepareAndStream(t, path, ModeStrip)
	if p.Format != wire.FormatJPEG || p.Width != 40 || p.Height != 30 {
		t.Fatalf("%+v", p)
	}
	assertNoSecrets(t, out)
	if !bytes.Equal(out[2:2+len(minimalJFIF)], minimalJFIF) || bytes.Contains(out, []byte("TTTT")) {
		t.Fatal("APP0 not rewritten to the minimal JFIF")
	}
	orig, _, err := Decode(context.Background(), dirtyJPEG(t, img, 1))
	if err != nil {
		t.Fatal(err)
	}
	got, info, err := Decode(context.Background(), out)
	if err != nil || info.Format != wire.FormatJPEG {
		t.Fatal(err)
	}
	samePixels(t, orig, got) // scan data untouched → identical pixels
	// Deterministic: a second Prepare gives the same hash.
	p2, _ := Prepare(context.Background(), path, ModeStrip, 0)
	if p2.Hash != p.Hash || p2.Size != p.Size {
		t.Fatal("not deterministic")
	}
	// Idempotent: stripping the output changes nothing.
	_, again := prepareAndStream(t, writeTemp(t, "b.jpg", out), ModeStrip)
	if !bytes.Equal(again, out) {
		t.Fatal("strip not idempotent")
	}
}

func TestJPEGOrientationReencodes(t *testing.T) {
	img := testImage(40, 30)
	for o := 2; o <= 8; o++ {
		path := writeTemp(t, "o.jpg", dirtyJPEG(t, img, uint16(o)))
		p, out := prepareAndStream(t, path, ModeStrip)
		if p.Format != wire.FormatJPEG {
			t.Fatal("format")
		}
		assertNoSecrets(t, out)
		got, _, err := Decode(context.Background(), out)
		if err != nil {
			t.Fatal(err)
		}
		want := applyOrientation(img, o)
		if got.Bounds() != want.Bounds() || p.Width != want.Bounds().Dx() || p.Height != want.Bounds().Dy() {
			t.Fatalf("o=%d bounds %v want %v (%d×%d)", o, got.Bounds(), want.Bounds(), p.Width, p.Height)
		}
		// Near-identical after a JPEG round trip: the white marker pixel must land in the right corner.
		wc := color.NRGBAModel.Convert(want.At(want.Bounds().Min.X, want.Bounds().Min.Y)).(color.NRGBA)
		gc := color.NRGBAModel.Convert(got.At(got.Bounds().Min.X, got.Bounds().Min.Y)).(color.NRGBA)
		if abs(int(wc.R)-int(gc.R)) > 40 || abs(int(wc.G)-int(gc.G)) > 40 {
			t.Fatalf("o=%d corner %v vs %v", o, wc, gc)
		}
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestPNGStrip(t *testing.T) {
	img := testImage(33, 21)
	path := writeTemp(t, "a.png", dirtyPNG(t, img, 1))
	p, out := prepareAndStream(t, path, ModeStrip)
	if p.Format != wire.FormatPNG {
		t.Fatal("format")
	}
	assertNoSecrets(t, out)
	for _, ch := range []string{"tEXt", "iTXt", "tIME", "pHYs", "iCCP", "eXIf"} {
		if bytes.Contains(out, []byte(ch)) {
			t.Fatalf("%s survived", ch)
		}
	}
	got, _, err := Decode(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	samePixels(t, img, got)
	// Bad CRC → corrupt.
	bad := dirtyPNG(t, img, 1)
	bad[len(bad)-len(trailing)-5] ^= 1 // inside IEND's CRC
	if _, err := Prepare(context.Background(), writeTemp(t, "bad.png", bad), ModeStrip, 0); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	// Orientation via eXIf → re-encoded PNG, still PNG, rotated.
	p6, out6 := prepareAndStream(t, writeTemp(t, "r.png", dirtyPNG(t, img, 6)), ModeStrip)
	got6, _, _ := Decode(context.Background(), out6)
	if p6.Format != wire.FormatPNG || got6.Bounds().Dx() != 21 || got6.Bounds().Dy() != 33 {
		t.Fatalf("%+v %v", p6, got6.Bounds())
	}
	samePixels(t, applyOrientation(img, 6), got6)
}

func TestGIFStrip(t *testing.T) {
	frames := []*image.Paletted{palettedFrame(16, 12, color.RGBA{200, 0, 0, 255}), palettedFrame(16, 12, color.RGBA{0, 200, 0, 255}), palettedFrame(16, 12, color.RGBA{0, 0, 200, 255})}
	path := writeTemp(t, "a.gif", dirtyGIF(t, frames))
	p, out := prepareAndStream(t, path, ModeStrip)
	if p.Format != wire.FormatGIF || !p.Animated {
		t.Fatalf("%+v", p)
	}
	assertNoSecrets(t, out)
	if !bytes.Contains(out, []byte("NETSCAPE2.0")) {
		t.Fatal("loop extension dropped")
	}
	g, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil || len(g.Image) != 3 {
		t.Fatal(err)
	}
	orig, _ := gif.DecodeAll(bytes.NewReader(dirtyGIF(t, frames)))
	for i := range g.Image {
		samePixels(t, orig.Image[i], g.Image[i])
	}
	info, err := Probe(out)
	if err != nil || info.Frames != 3 || !info.Animated {
		t.Fatalf("%+v %v", info, err)
	}
	// Static GIF.
	ps, _ := prepareAndStream(t, writeTemp(t, "s.gif", dirtyGIF(t, frames[:1])), ModeStrip)
	if ps.Animated {
		t.Fatal("static reported animated")
	}
}

func TestGIFFrameLimits(t *testing.T) {
	var frames []*image.Paletted
	for i := 0; i < wire.MaxGIFFrames+1; i++ {
		frames = append(frames, palettedFrame(2, 2, color.RGBA{1, 2, 3, 255}))
	}
	data := dirtyGIF(t, frames)
	if _, err := Probe(data); !errors.Is(err, ErrTooLarge) {
		t.Fatal("201 frames accepted", err)
	}
	if _, err := Probe(dirtyGIF(t, frames[:wire.MaxGIFFrames])); err != nil {
		t.Fatal(err)
	}
	// Frame area sum over the limit with few frames: 3 frames of 4000×4000 = 48M > 40M.
	var big []*image.Paletted
	for i := 0; i < 3; i++ {
		big = append(big, palettedFrame(4000, 4000, color.RGBA{1, 2, 3, 255}))
	}
	bigData := dirtyGIF(t, big)
	if _, err := Probe(bigData); !errors.Is(err, ErrTooLarge) {
		t.Fatal("frame area accepted", err)
	}
}

func TestWebPStrip(t *testing.T) {
	c := color.NRGBA{10, 200, 30, 255}
	plain := webpFile(5, 3, c, false, 1, false)
	img, info, err := Decode(context.Background(), plain)
	if err != nil || info.Format != wire.FormatWebP || img.Bounds().Dx() != 5 {
		t.Fatalf("hand-built VP8L does not decode: %v %+v", err, info)
	}
	if got := color.NRGBAModel.Convert(img.At(2, 1)); got != c {
		t.Fatalf("color %v", got)
	}
	dirty := webpFile(5, 3, c, true, 1, false)
	if _, _, err := Decode(context.Background(), dirty); err != nil {
		t.Fatal(err)
	}
	p, out := prepareAndStream(t, writeTemp(t, "a.webp", dirty), ModeStrip)
	if p.Format != wire.FormatWebP {
		t.Fatal("format")
	}
	assertNoSecrets(t, out)
	if bytes.Contains(out, []byte("ICCP")) || bytes.Contains(out, []byte("EXIF")) || bytes.Contains(out, []byte("XMP ")) {
		t.Fatal("metadata chunk survived")
	}
	if out[20]&(vp8xICC|vp8xEXIF|vp8xXMP) != 0 {
		t.Fatalf("VP8X flags not cleared: %02x", out[20])
	}
	got, _, err := Decode(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	samePixels(t, img, got)
	// Oriented WebP comes out as PNG.
	p6, out6 := prepareAndStream(t, writeTemp(t, "r.webp", webpFile(5, 3, c, true, 6, false)), ModeStrip)
	if p6.Format != wire.FormatPNG {
		t.Fatalf("rotated webp format %d", p6.Format)
	}
	got6, info6, _ := Decode(context.Background(), out6)
	if info6.Format != wire.FormatPNG || got6.Bounds().Dx() != 3 || got6.Bounds().Dy() != 5 {
		t.Fatal("rotation")
	}
	// Animated WebP rejected everywhere.
	anim := webpFile(5, 3, c, true, 1, true)
	if _, err := Probe(anim); !errors.Is(err, ErrAnimated) {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), writeTemp(t, "an.webp", anim), ModeStrip, 0); !errors.Is(err, ErrAnimated) {
		t.Fatal(err)
	}
}

func TestParanoid(t *testing.T) {
	img := testImage(24, 16)
	for name, data := range map[string][]byte{"j.jpg": dirtyJPEG(t, img, 1), "p.png": dirtyPNG(t, img, 3), "w.webp": webpFile(4, 4, color.NRGBA{1, 2, 3, 255}, true, 1, false)} {
		p, out := prepareAndStream(t, writeTemp(t, name, data), ModeParanoid)
		if p.Format != wire.FormatPNG {
			t.Fatalf("%s: paranoid format %d", name, p.Format)
		}
		assertNoSecrets(t, out)
		if _, info, err := Decode(context.Background(), out); err != nil || info.Format != wire.FormatPNG {
			t.Fatal(name, err)
		}
	}
	// Animated GIF stays GIF in paranoid mode (pixels and delays only).
	frames := []*image.Paletted{palettedFrame(8, 8, color.RGBA{9, 9, 9, 255}), palettedFrame(8, 8, color.RGBA{1, 1, 1, 255})}
	p, out := prepareAndStream(t, writeTemp(t, "a.gif", dirtyGIF(t, frames)), ModeParanoid)
	if p.Format != wire.FormatGIF {
		t.Fatal("gif")
	}
	assertNoSecrets(t, out)
	if g, err := gif.DecodeAll(bytes.NewReader(out)); err != nil || len(g.Image) != 2 {
		t.Fatal(err)
	}
}

func TestGatesAndLies(t *testing.T) {
	// Declared bomb dimensions: a PNG IHDR claiming 20000×20000.
	var buf bytes.Buffer
	_ = png.Encode(&buf, testImage(2, 2))
	bomb := buf.Bytes()
	bomb[16], bomb[17], bomb[18], bomb[19] = 0, 0, 0x4E, 0x20 // width 20000
	bomb[20], bomb[21], bomb[22], bomb[23] = 0, 0, 0x4E, 0x20
	// Fix the IHDR CRC so only the dimension gate can reject it.
	fixed := append([]byte(nil), bomb[:8]...)
	fixed = append(fixed, pngChunk("IHDR", bomb[16:29])...)
	fixed = append(fixed, bomb[33:]...)
	if _, err := Probe(fixed); !errors.Is(err, ErrTooLarge) {
		t.Fatal("bomb accepted", err)
	}
	if err := CheckDimensions(16385, 1); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if err := CheckDimensions(8000, 5001); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if err := CheckDimensions(0, 5); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	if err := CheckDimensions(6000, 6000); err != nil {
		t.Fatal(err)
	}
	// A file whose extension disagrees with its magic bytes is rejected; no extension cannot disagree.
	for _, name := range []string{"lie.jpg", "lie.gif", "lie.txt", "lie.PNG.exe"} {
		if _, err := ProbeFile(writeTemp(t, name, dirtyPNG(t, testImage(4, 4), 1))); !errors.Is(err, ErrMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"ok.png", "OK.PNG", "noext"} {
		info, err := ProbeFile(writeTemp(t, name, dirtyPNG(t, testImage(4, 4), 1)))
		if err != nil || info.Format != wire.FormatPNG {
			t.Fatal(name, info, err)
		}
	}
	if _, err := ProbeFile(writeTemp(t, "photo.jpeg", dirtyJPEG(t, testImage(4, 4), 1))); err != nil {
		t.Fatal(err)
	}
	// Truncated files are corrupt, not panics.
	for _, data := range [][]byte{dirtyJPEG(t, testImage(8, 8), 1)[:40], dirtyPNG(t, testImage(8, 8), 1)[:40], dirtyGIF(t, []*image.Paletted{palettedFrame(3, 3, color.Black)})[:20]} {
		if _, err := Prepare(context.Background(), writeTemp(t, "t.bin", data), ModeStrip, 0); err == nil {
			t.Fatal("truncated accepted")
		}
	}
	if _, err := Prepare(context.Background(), writeTemp(t, "x.txt", []byte("not an image")), ModeStrip, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	// maxSize enforced on the stripped output.
	if _, err := Prepare(context.Background(), writeTemp(t, "m.png", dirtyPNG(t, testImage(64, 64), 1)), ModeStrip, 100); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestStreamDetectsChange(t *testing.T) {
	img := testImage(30, 30)
	path := writeTemp(t, "c.png", dirtyPNG(t, img, 1))
	p, err := Prepare(context.Background(), path, ModeStrip, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, dirtyPNG(t, testImage(30, 31), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Stream(context.Background(), p, func(uint32, []byte) error { return nil })
	if !errors.Is(err, ErrDiverged) {
		t.Fatal(err)
	}
}

func TestChunking(t *testing.T) {
	// A PNG larger than one chunk streams as ChunkData-sized pieces plus a remainder.
	img := image.NewNRGBA(image.Rect(0, 0, 600, 600))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 7)
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	path := writeTemp(t, "big.png", buf.Bytes())
	p, err := Prepare(context.Background(), path, ModeStrip, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Size <= wire.ChunkData {
		t.Skip("fixture too small")
	}
	var sizes []int
	if err := Stream(context.Background(), p, func(i uint32, c []byte) error { sizes = append(sizes, len(c)); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, s := range sizes[:len(sizes)-1] {
		if s != wire.ChunkData {
			t.Fatal(s)
		}
	}
	if last := sizes[len(sizes)-1]; last == 0 || last > wire.ChunkData || uint64(last) != p.Size-uint64(wire.ChunkData)*uint64(len(sizes)-1) {
		t.Fatal(last)
	}
}

func TestThumbnail(t *testing.T) {
	th := Thumbnail(testImage(400, 200), 100)
	if th.Bounds().Dx() != 100 || th.Bounds().Dy() != 50 {
		t.Fatal(th.Bounds())
	}
	small := testImage(10, 10)
	if Thumbnail(small, 100) != small {
		t.Fatal("upscaled")
	}
	if b := Thumbnail(testImage(1000, 2), 100).Bounds(); b.Dx() != 100 || b.Dy() != 1 {
		t.Fatal(b)
	}
}

func TestExifOrientationParser(t *testing.T) {
	if exifOrientation(tiffWithOrientation(6)) != 6 {
		t.Fatal("LE")
	}
	be := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x08\x00\x00\x00\x00\x00\x00")
	if exifOrientation(be) != 8 {
		t.Fatal("BE")
	}
	for _, bad := range [][]byte{nil, []byte("II"), []byte("XX\x2a\x00\x08\x00\x00\x00"), []byte("II\x2a\x00\xff\xff\xff\xff"), tiffWithOrientation(9), tiffWithOrientation(0)} {
		if exifOrientation(bad) != 0 {
			t.Fatalf("accepted %x", bad)
		}
	}
}

// --- fuzzers (§13.4) ---

func fuzzStrip(f *testing.F, seeds [][]byte, strip func([]byte) ([]byte, error)) {
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := strip(data)
		if err != nil {
			return
		}
		again, err := strip(out)
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("strip not idempotent: %v", err)
		}
	})
}

func FuzzJPEGStrip(f *testing.F) {
	img := testImage(8, 6)
	fuzzStrip(f, [][]byte{dirtyJPEG(f, img, 1), dirtyJPEG(f, img, 6), []byte("\xff\xd8\xff\xd9")},
		func(b []byte) ([]byte, error) {
			var out bytes.Buffer
			_, err := stripJPEG(&out, bytes.NewReader(b))
			return out.Bytes(), err
		})
}

func FuzzPNGStrip(f *testing.F) {
	img := testImage(8, 6)
	fuzzStrip(f, [][]byte{dirtyPNG(f, img, 1), dirtyPNG(f, img, 6)},
		func(b []byte) ([]byte, error) {
			var out bytes.Buffer
			_, err := stripPNG(&out, bytes.NewReader(b))
			return out.Bytes(), err
		})
}

func FuzzWebPStrip(f *testing.F) {
	c := color.NRGBA{1, 2, 3, 255}
	fuzzStrip(f, [][]byte{webpFile(4, 4, c, false, 1, false), webpFile(4, 4, c, true, 6, false), webpFile(4, 4, c, true, 1, true)},
		func(b []byte) ([]byte, error) {
			var out bytes.Buffer
			_, err := stripWebP(&out, bytes.NewReader(b))
			return out.Bytes(), err
		})
}

func FuzzGIFStrip(f *testing.F) {
	frames := []*image.Paletted{palettedFrame(4, 4, color.RGBA{1, 2, 3, 255}), palettedFrame(4, 4, color.RGBA{3, 2, 1, 255})}
	fuzzStrip(f, [][]byte{dirtyGIF(f, frames), dirtyGIF(f, frames[:1])},
		func(b []byte) ([]byte, error) {
			var out bytes.Buffer
			err := stripGIF(&out, bytes.NewReader(b))
			return out.Bytes(), err
		})
}

func FuzzGIFPrescan(f *testing.F) {
	frames := []*image.Paletted{palettedFrame(4, 4, color.RGBA{1, 2, 3, 255})}
	f.Add(dirtyGIF(f, frames))
	f.Add([]byte("GIF89a"))
	f.Fuzz(func(t *testing.T, data []byte) {
		frames, area, err := gifPrescan(bytes.NewReader(data))
		if err == nil && (frames < 0 || area < 0) {
			t.Fatal("negative")
		}
		_, _ = Probe(data) // must never panic
		// The limits hold only if the decoder makes exactly the frames the
		// pre-scan counted. (A small area keeps the decode cheap.)
		if err == nil && area <= 1<<16 {
			if g, derr := gif.DecodeAll(bytes.NewReader(data)); derr == nil && len(g.Image) != frames {
				t.Fatalf("pre-scan counted %d frames, the decoder made %d", frames, len(g.Image))
			}
		}
	})
}
