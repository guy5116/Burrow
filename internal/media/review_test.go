package media

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
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
	if err := stripGIF(&out, bytes.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("HIDDEN")) {
		t.Fatal("a note survived under the graphic control label")
	}
	if pngKeep("gAMA", 4) != true || pngKeep("gAMA", 40) || pngKeep("sRGB", 2) || !pngKeep("sRGB", 1) || pngKeep("tEXt", 4) {
		t.Fatal("pngKeep")
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
