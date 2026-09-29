package media

import (
	"bytes"
	"image"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func noisy(w, h int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	x := uint32(2463534242)
	for i := range img.Pix {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		img.Pix[i] = byte(x)
	}
	return img
}

func BenchmarkStripJPEG(b *testing.B) {
	data := dirtyJPEG(b, noisy(1600, 1200), 1)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := stripJPEG(io.Discard, bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStripPNG(b *testing.B) {
	data := dirtyPNG(b, noisy(1200, 900), 1)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := stripPNG(io.Discard, bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPrepare is pass 1 of the sending pipeline: gates, strip, hash.
func BenchmarkPrepare(b *testing.B) {
	data := dirtyJPEG(b, noisy(1600, 1200), 1)
	path := filepath.Join(b.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := Prepare(path, ModeStrip, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// TestWriteFuzzSeeds regenerates the committed seed corpora under
// testdata/fuzz (run with BURROW_WRITE_SEEDS=1). The fixtures are generated
// here, never downloaded.
func TestWriteFuzzSeeds(t *testing.T) {
	if os.Getenv("BURROW_WRITE_SEEDS") == "" {
		t.Skip("set BURROW_WRITE_SEEDS=1 to regenerate the seed corpora")
	}
	img := testImage(8, 6)
	frames := []*image.Paletted{palettedFrame(4, 4, image.Black), palettedFrame(4, 4, image.White)}
	seeds := map[string][][]byte{
		"FuzzJPEGStrip":  {dirtyJPEG(t, img, 1), dirtyJPEG(t, img, 6)},
		"FuzzPNGStrip":   {dirtyPNG(t, img, 1), dirtyPNG(t, img, 8)},
		"FuzzWebPStrip":  {webpFile(4, 4, testColor, true, 1, false), webpFile(4, 4, testColor, true, 1, true)},
		"FuzzGIFStrip":   {dirtyGIF(t, frames), dirtyGIF(t, frames[:1])},
		"FuzzGIFPrescan": {dirtyGIF(t, frames)},
	}
	for name, list := range seeds {
		dir := filepath.Join("testdata", "fuzz", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i, data := range list {
			body := "go test fuzz v1\n[]byte(" + strconv.Quote(string(data)) + ")\n"
			if err := os.WriteFile(filepath.Join(dir, "seed-"+strconv.Itoa(i)), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
