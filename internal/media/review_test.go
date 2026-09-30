package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"os"
	"runtime"
	"testing"

	"github.com/guy5116/burrow/internal/wire"
)

// allocated returns the bytes fn allocates.
func allocated(fn func()) uint64 {
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	fn()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// A length field in a tiny file must not decide how much memory is taken.
func TestLengthFieldsDoNotAllocate(t *testing.T) {
	png := append([]byte(nil), dirtyPNG(t, testImage(1, 1), 1)[:8+8+13+4]...) // signature + IHDR
	png = append(png, 0x7F, 0xFF, 0xFF, 0xFF, 't', 'E', 'X', 't')
	webp := []byte("RIFF\xfe\xff\xff\x7fWEBP")
	webp = append(webp, riffChunk("VP8X", []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0})...)
	webp = append(webp, 'J', 'U', 'N', 'K', 0, 0, 0, 0x70)
	for name, strip := range map[string]func() error{
		"png":  func() error { _, err := stripPNG(&bytes.Buffer{}, bytes.NewReader(png)); return err },
		"webp": func() error { _, err := stripWebP(&bytes.Buffer{}, bytes.NewReader(webp)); return err },
	} {
		var err error
		if n := allocated(func() { err = strip() }); n > 1<<20 {
			t.Errorf("%s: allocated %d bytes for a file of a few dozen", name, n)
		}
		if !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// WebP stripping keeps the image chunks and nothing else.
func TestWebPDropsUnknownChunks(t *testing.T) {
	body := riffChunk("VP8L", vp8l(2, 2, color.NRGBA{9, 9, 9, 255}))
	body = append(body, riffChunk("MeTa", []byte("GPS=51.5007,-0.1246 author=alice"))...)
	file := append([]byte("RIFF\x00\x00\x00\x00WEBP"), body...)
	binary.LittleEndian.PutUint32(file[4:], uint32(4+len(body)))
	var out bytes.Buffer
	if _, err := stripWebP(&out, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("GPS=")) || !bytes.Contains(out.Bytes(), []byte("VP8L")) {
		t.Fatalf("%q", out.Bytes())
	}
	if got := binary.LittleEndian.Uint32(out.Bytes()[4:]); int(got) != out.Len()-8 {
		t.Fatalf("RIFF size %d for %d bytes", got, out.Len())
	}
	if _, err := Probe(out.Bytes()); err != nil {
		t.Fatal(err)
	}
}

// Blocks that are kept are kept only in their legal shape.
func TestKeptBlocksHaveOneShape(t *testing.T) {
	gifData := dirtyGIF(t, []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 2, 2), color.Palette{color.Black, color.White})})
	// A "graphic control" block that is really a note, before the first image.
	fake := append([]byte{0x21, 0xF9, 22}, []byte("HIDDEN-IN-GRAPHIC-CTRL")...)
	fake = append(fake, 0)
	at := bytes.IndexByte(gifData[13+6:], 0x2C) + 13 + 6
	in := append(append(append([]byte(nil), gifData[:at]...), fake...), gifData[at:]...)
	var out bytes.Buffer
	if err := stripGIF(&out, bytes.NewReader(in)); !errors.Is(err, ErrCorrupt) || bytes.Contains(out.Bytes(), []byte("HIDDEN")) {
		t.Fatal("a graphic control block of the wrong shape was not refused", err) // image/gif refuses it too
	}
	for _, c := range []struct {
		typ  string
		n    uint32
		keep bool
	}{
		{"gAMA", 4, true}, {"gAMA", 40, false}, {"sRGB", 1, true}, {"sRGB", 2, false}, {"tEXt", 4, false},
		{"IEND", 0, true}, {"IEND", 27, false}, {"PLTE", 6, true}, {"PLTE", 7, false}, {"PLTE", 0, false},
		{"PLTE", 771, false}, {"tRNS", 256, true}, {"tRNS", 257, false},
	} {
		if pngKeep(c.typ, c.n) != c.keep {
			t.Errorf("pngKeep(%s, %d)", c.typ, c.n)
		}
	}
	if !gifKeepExtension(0xF9, []byte{4, 0, 0, 0, 0, 0}) || gifKeepExtension(0xFE, []byte{0}) ||
		!gifKeepExtension(0xFF, append(append([]byte{11}, "NETSCAPE2.0"...), 3, 1, 0, 0, 0)) ||
		gifKeepExtension(0xFF, append(append([]byte{11}, "NETSCAPE2.0"...), 3, 1, 0, 0, 5)) {
		t.Fatal("gifKeepExtension")
	}
}

// The sink never sees a chunk the offer did not announce, whether the file
// grew or shrank after pass 1.
func TestStreamKeepsChunkShape(t *testing.T) {
	path := writeTemp(t, "data.bin", bytes.Repeat([]byte{7}, 1000))
	p, err := PrepareFile(context.Background(), path, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, size := range map[string]int{"grown": 3 * wire.ChunkData, "shrunk": 10} {
		if err := os.WriteFile(path, bytes.Repeat([]byte{7}, size), 0o600); err != nil {
			t.Fatal(err)
		}
		calls := 0
		err := Stream(context.Background(), p, func(uint32, []byte) error { calls++; return nil })
		if !errors.Is(err, ErrDiverged) || calls != 0 {
			t.Errorf("%s: %d chunks handed over, err %v", name, calls, err)
		}
	}
}

// Each EXIF orientation against a grid written out by hand: pixel values are
// the positions 1–6 of a 2×3 image, read row by row.
func TestOrientationGrid(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 3))
	for i := 0; i < 6; i++ {
		src.Pix[i*4], src.Pix[i*4+3] = byte(i+1), 255
	}
	want := map[int][][]byte{
		1: {{1, 2}, {3, 4}, {5, 6}},
		2: {{2, 1}, {4, 3}, {6, 5}}, // mirrored left to right
		3: {{6, 5}, {4, 3}, {2, 1}}, // rotated 180°
		4: {{5, 6}, {3, 4}, {1, 2}}, // mirrored top to bottom
		5: {{1, 3, 5}, {2, 4, 6}},   // transposed
		6: {{5, 3, 1}, {6, 4, 2}},   // rotated 90° clockwise
		7: {{6, 4, 2}, {5, 3, 1}},   // transversed
		8: {{2, 4, 6}, {1, 3, 5}},   // rotated 90° counter-clockwise
	}
	for o, grid := range want {
		got := applyOrientation(src, o)
		if got.Bounds().Dx() != len(grid[0]) || got.Bounds().Dy() != len(grid) {
			t.Errorf("orientation %d: bounds %v", o, got.Bounds())
			continue
		}
		for y, row := range grid {
			for x, v := range row {
				if r, _, _, _ := got.At(got.Bounds().Min.X+x, got.Bounds().Min.Y+y).RGBA(); byte(r>>8) != v {
					t.Errorf("orientation %d at (%d,%d): %d want %d", o, x, y, r>>8, v)
				}
			}
		}
	}
}

// Pass 1 over a large file stops when its context ends.
func TestPrepareStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	path := writeTemp(t, "big.bin", make([]byte, 1<<20))
	if _, err := PrepareFile(ctx, path, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Prepare(ctx, writeTemp(t, "a.png", dirtyPNG(t, testImage(4, 4), 1)), ModeStrip, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Waiting for a decode slot ends with the context too.
	decodeSem <- struct{}{}
	decodeSem <- struct{}{}
	_, _, err := Decode(ctx, dirtyPNG(t, testImage(4, 4), 1))
	<-decodeSem
	<-decodeSem
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// A progressive JPEG costs image/jpeg a coefficient block per 8×8 block of
// every component on top of the image, and a pass over the image per scan:
// the gate counts both.
func TestProgressiveJPEGBudget(t *testing.T) {
	const scan = "\xff\xda\x00\x08\x01\x01\x00\x00\x3f\x00" + "\x12\xff\x00\x34\xff\xd0\x56" // header, then data with stuffing and a restart
	jpeg := func(marker byte, w, h int, sampling []byte, scans int) []byte {
		b := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 4, 'x', 'x', 0xFF, marker, 0, byte(8 + 3*len(sampling)), 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(len(sampling))}
		for i, s := range sampling {
			b = append(b, byte(i+1), s, 0)
		}
		for i := 0; i < scans; i++ {
			b = append(b, scan...)
		}
		return append(b, 0xFF, 0xD9)
	}
	cmyk := []byte{0x11, 0x11, 0x11, 0x11}
	// 6320×6320 CMYK: 160 MB for the image, 640 MB more for the scans.
	if extra, err := jpegScanBytes(bytes.NewReader(jpeg(0xC2, 6320, 6320, cmyk, 3)), 6320, 6320); err != nil || extra != 6320*6320*16 {
		t.Fatal(extra, err)
	}
	if extra, err := jpegScanBytes(bytes.NewReader(jpeg(0xC0, 6320, 6320, cmyk, 1)), 6320, 6320); err != nil || extra != 0 {
		t.Fatal("baseline", extra, err)
	}
	// 4:2:0 YCbCr: luma at full resolution, chroma at a quarter.
	if extra, err := jpegScanBytes(bytes.NewReader(jpeg(0xC2, 16, 16, []byte{0x22, 0x11, 0x11}, 1)), 16, 16); err != nil || extra != (4+1+1)*256 {
		t.Fatal("4:2:0", extra, err)
	}
	if _, err := jpegScanBytes(bytes.NewReader(jpeg(0xC2, 16, 16, cmyk, maxJPEGScans)), 16, 16); err != nil {
		t.Fatal(err)
	}
	if _, err := jpegScanBytes(bytes.NewReader(jpeg(0xC2, 16, 16, cmyk, maxJPEGScans+1)), 16, 16); !errors.Is(err, ErrTooLarge) {
		t.Fatal("too many scans", err)
	}
	for _, bad := range [][]byte{nil, {0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xDA, 0, 2}, {0xFF, 0xD8, 0xFF, 0xC2, 0, 8, 8, 0}, jpeg(0xC2, 16, 16, cmyk, 1)[:40]} {
		if _, err := jpegScanBytes(bytes.NewReader(bad), 1, 1); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%x: %v", bad, err)
		}
	}
}

func TestKeepsMetadata(t *testing.T) {
	for head, want := range map[string]bool{
		"\x00\x00\x00\x18ftypheic": true, "\x00\x00\x00\x1cftypavif": true, "\x00\x00\x00\x14ftypqt  ": true,
		"II*\x00\x08\x00": true, "MM\x00*\x00\x00": true, "\xff\x0a\xff": true, "\x1a\x45\xdf\xa3": true,
		"RIFF\x00\x00\x00\x00AVI ": true, "RIFF\x00\x00\x00\x00WEBP": false, "PK\x03\x04": false, "%PDF-1.7": false, "": false,
	} {
		if got := KeepsMetadata([]byte(head)) != ""; got != want {
			t.Errorf("%q: %v", head, got)
		}
	}
}

// Sent as a file, an image is stripped whatever its extension says.
func TestPrepareByContent(t *testing.T) {
	path := writeTemp(t, "scan.jfif", dirtyPNG(t, testImage(4, 4), 1))
	if _, err := Prepare(context.Background(), path, ModeStrip, 0); !errors.Is(err, ErrMismatch) {
		t.Fatal(err)
	}
	p, err := PrepareByContent(context.Background(), path, ModeStrip, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Stream(context.Background(), p, func(_ uint32, c []byte) error { out.Write(c); return nil }); err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, out.Bytes())
}

// GIF extensions are walked exactly as image/gif walks them, so the frames
// the pre-scan counts are the frames the decoder makes.
func TestGIFExtensionsAsTheDecoderReadsThem(t *testing.T) {
	two := dirtyGIF(t, []*image.Paletted{palettedFrame(2, 2, color.RGBA{1, 2, 3, 255}), palettedFrame(2, 2, color.RGBA{4, 5, 6, 255})})
	at := bytes.IndexByte(two[13+6:], 0x2C) + 13 + 6
	with := func(ext []byte) []byte {
		return append(append(append([]byte(nil), two[:at]...), ext...), two[at:]...)
	}
	for name, ext := range map[string][]byte{
		// image/gif reads 13 fixed bytes: a first byte other than 12 would
		// make the two walkers part ways.
		"plain text of another length":      {0x21, 0x01, 0x00, 0x3B, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 0},
		"unknown label":                     {0x21, 0x02, 0x01, 0x00, 0x00},
		"empty application identifier":      {0x21, 0xFF, 0x00, 0x01, 0x00, 0x00},
		"graphic control of another length": {0x21, 0xF9, 0x05, 0, 0, 0, 0, 0, 0},
	} {
		if _, _, err := gifPrescan(bytes.NewReader(with(ext))); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
	plain := append([]byte{0x21, 0x01, 12}, make([]byte, 12)...)
	plain = append(plain, 3, 'a', 'b', 'c', 0)
	frames, _, err := gifPrescan(bytes.NewReader(with(plain)))
	g, derr := gif.DecodeAll(bytes.NewReader(with(plain)))
	if err != nil || derr != nil || frames != len(g.Image) {
		t.Fatal(frames, err, derr)
	}
}

// Kept WebP chunks carry nothing beyond what the format defines: the VP8X
// reserved bits and bytes and the pad byte of an odd chunk are zero.
func TestWebPKeptChunksAreClean(t *testing.T) {
	vp8x := []byte{0xFF, 0xAA, 0xBB, 0xCC, 1, 0, 0, 1, 0, 0} // every flag and reserved bit set
	vp8x[0] &^= vp8xAnim
	alph := []byte("odd") // an odd length: one pad byte follows
	body := riffChunk("VP8X", vp8x)
	chunk := riffChunk("ALPH", alph)
	chunk[len(chunk)-1] = 0x5A // a pad byte that says something
	body = append(body, chunk...)
	body = append(body, riffChunk("VP8L", vp8l(2, 2, color.NRGBA{9, 9, 9, 255}))...)
	file := append([]byte("RIFF\x00\x00\x00\x00WEBP"), body...)
	binary.LittleEndian.PutUint32(file[4:], uint32(4+len(body)))
	var out bytes.Buffer
	if _, err := stripWebP(&out, bytes.NewReader(file)); err != nil {
		t.Fatal(err)
	}
	o := out.Bytes()
	x := o[12+8 : 12+8+vp8xLen]
	if x[0] != vp8xAlpha || x[1]|x[2]|x[3] != 0 {
		t.Fatalf("VP8X %x", x)
	}
	a := bytes.Index(o, []byte("ALPH"))
	if o[a+8+3] != 0 {
		t.Fatal("pad byte copied")
	}
	short := riffChunk("VP8X", vp8x[:8])
	bad := append([]byte("RIFF\x00\x00\x00\x00WEBP"), short...)
	binary.LittleEndian.PutUint32(bad[4:], uint32(4+len(short)))
	if _, err := stripWebP(&out, bytes.NewReader(bad)); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

// Only the colour transform of an Adobe segment is kept, in a fixed form.
func TestAdobeSegmentIsCanonical(t *testing.T) {
	long := append([]byte("Adobe\x00\x65\x80\x00\x00\x00\x02"), []byte("made with SecretCam 3.1")...)
	if got := adobeSegment(long); !bytes.Equal(got, []byte("\xff\xee\x00\x0eAdobe\x00\x64\x00\x00\x00\x00\x02")) {
		t.Fatalf("%q", got)
	}
	for _, bad := range [][]byte{[]byte("Adobe"), []byte("Adobx\x00\x64\x00\x00\x00\x00\x01"), []byte("Adobe\x00\x64\x00\x00\x00\x00\x07")} {
		if adobeSegment(bad) != nil {
			t.Errorf("%q kept", bad)
		}
	}
}
