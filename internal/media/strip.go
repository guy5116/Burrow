package media

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"

	"golang.org/x/crypto/blake2b"

	"github.com/guy5116/burrow/internal/wire"
)

// Mode selects strip (lossless, default) or paranoid (decode + re-encode).
type Mode uint8

// Modes.
const (
	ModeStrip Mode = iota
	ModeParanoid
)

const jpegQuality = 92

// Prepared is the result of pass 1: what the IMG_OFFER announces.
type Prepared struct {
	Format   Format // output format (may differ from the file's)
	Size     uint64
	Hash     [wire.HashSize]byte
	Width    int
	Height   int
	Animated bool
	Mode     Mode
	Path     string
}

// ProbeFile sniffs and gates a file without decoding pixels.
func ProbeFile(path string) (Info, error) {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	head := make([]byte, SniffLen)
	n, _ := io.ReadFull(f, head)
	if _, err := Sniff(head[:n]); err != nil {
		return Info{}, err
	}
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if st.Size() > int64(wire.MaxImageDecodeBytes) {
		return Info{}, ErrTooLarge
	}
	data, err := readAll(io.NewSectionReader(f, 0, st.Size()), int64(wire.MaxImageDecodeBytes))
	if err != nil {
		return Info{}, err
	}
	return Probe(data)
}

// Prepare runs pass 1 over path: gates, strips (or re-encodes), hashes, and
// records size and format for the offer. Nothing is written to disk and, in
// strip mode without rotation, the file is never held in memory.
func Prepare(path string, mode Mode, maxSize uint64) (Prepared, error) {
	info, err := ProbeFile(path)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Width: info.Width, Height: info.Height, Animated: info.Animated, Mode: mode, Path: path}
	o := orientationOfFile(path, info)
	if orientationSwaps(o) {
		p.Width, p.Height = info.Height, info.Width
	}
	h, _ := blake2b.New256(nil)
	cw := &countWriter{w: h}
	p.Format, err = process(path, info, mode, o, cw)
	if err != nil {
		return Prepared{}, err
	}
	if maxSize > 0 && cw.n > maxSize {
		return Prepared{}, ErrTooLarge
	}
	p.Size = cw.n
	copy(p.Hash[:], h.Sum(nil))
	return p, nil
}

// Stream runs pass 2: strips again while handing ChunkData-sized chunks to
// sink (the last one shorter). If the output diverges from pass 1, it returns
// ErrDiverged after the last chunk.
func Stream(p Prepared, sink func(index uint32, chunk []byte) error) error {
	info, err := ProbeFile(p.Path)
	if err != nil {
		return err
	}
	h, _ := blake2b.New256(nil)
	ck := &chunker{sink: sink}
	format, err := process(p.Path, info, p.Mode, orientationOfFile(p.Path, info), io.MultiWriter(h, ck))
	if err != nil {
		return err
	}
	if err := ck.flush(); err != nil {
		return err
	}
	if format != p.Format || ck.total != p.Size || subtle.ConstantTimeCompare(h.Sum(nil), p.Hash[:]) != 1 {
		return ErrDiverged
	}
	return nil
}

// ErrDiverged means the second stripping pass produced different bytes.
var ErrDiverged = errBase("media: file changed between passes")

type errBase string

func (e errBase) Error() string { return string(e) }

// process writes the stripped or re-encoded image to w and returns the output format.
func process(path string, info Info, mode Mode, orientation int, w io.Writer) (Format, error) {
	if mode == ModeParanoid || orientation > 1 {
		return reencode(path, info, mode, orientation, w)
	}
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	switch info.Format {
	case wire.FormatJPEG:
		_, err = stripJPEG(w, r)
	case wire.FormatPNG:
		_, err = stripPNG(w, r)
	case wire.FormatWebP:
		_, err = stripWebP(w, r)
	case wire.FormatGIF:
		err = stripGIF(w, r)
	default:
		err = ErrUnsupported
	}
	return info.Format, err
}

// orientationOfFile runs the streaming stripper with a discarded output to
// learn the EXIF orientation without decoding pixels.
func orientationOfFile(path string, info Info) int {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return 0
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var o int
	switch info.Format {
	case wire.FormatJPEG:
		o, _ = stripJPEG(io.Discard, r)
	case wire.FormatPNG:
		o, _ = stripPNG(io.Discard, r)
	case wire.FormatWebP:
		o, _ = stripWebP(io.Discard, r)
	}
	return o
}

func orientationSwaps(o int) bool { return o >= 5 && o <= 8 }

// reencode decodes (bounded by the gates and the decode semaphore), applies
// the orientation, and encodes: PNG → PNG, JPEG → JPEG q92, WebP → PNG, GIF →
// GIF (animated keeps its frames and delays, nothing else). Paranoid mode
// always produces PNG except for animated GIFs.
func reencode(path string, info Info, mode Mode, orientation int, w io.Writer) (Format, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- user-chosen file; size gated by ProbeFile
	if err != nil {
		return 0, err
	}
	if info.Format == wire.FormatGIF && info.Animated {
		g, err := DecodeGIF(data)
		if err != nil {
			return 0, err
		}
		out := &gif.GIF{Image: g.Image, Delay: g.Delay, LoopCount: g.LoopCount, Disposal: g.Disposal, Config: g.Config}
		return wire.FormatGIF, gif.EncodeAll(w, out)
	}
	img, _, err := Decode(data)
	if err != nil {
		return 0, err
	}
	img = applyOrientation(img, orientation)
	switch {
	case mode == ModeParanoid, info.Format == wire.FormatPNG, info.Format == wire.FormatWebP, info.Format == wire.FormatGIF:
		enc := png.Encoder{CompressionLevel: png.DefaultCompression}
		return wire.FormatPNG, enc.Encode(w, img)
	default:
		return wire.FormatJPEG, jpeg.Encode(w, img, &jpeg.Options{Quality: jpegQuality})
	}
}

// applyOrientation maps EXIF orientations 2–8 onto the image.
func applyOrientation(img image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	dw, dh := w, h
	if orientationSwaps(o) {
		dw, dh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			si := src.PixOffset(x, y)
			di := dst.PixOffset(dx, dy)
			copy(dst.Pix[di:di+4], src.Pix[si:si+4])
		}
	}
	return dst
}

type countWriter struct {
	w io.Writer
	n uint64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += uint64(n) // #nosec G115 -- n ≥ 0
	return n, err
}

// chunker cuts a stream into ChunkData-sized chunks.
type chunker struct {
	sink  func(index uint32, chunk []byte) error
	buf   bytes.Buffer
	index uint32
	total uint64
}

func (c *chunker) Write(p []byte) (int, error) {
	n := len(p)
	c.total += uint64(n) // #nosec G115 -- n ≥ 0
	for len(p) > 0 {
		room := wire.ChunkData - c.buf.Len()
		take := min(room, len(p))
		c.buf.Write(p[:take])
		p = p[take:]
		if c.buf.Len() == wire.ChunkData {
			if err := c.emit(); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

func (c *chunker) emit() error {
	err := c.sink(c.index, c.buf.Bytes())
	c.index++
	c.buf.Reset()
	return err
}

func (c *chunker) flush() error {
	if c.buf.Len() == 0 {
		return nil
	}
	return c.emit()
}
