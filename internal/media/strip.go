package media

import (
	"bytes"
	"context"
	"crypto/subtle"
	"hash"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/blake2b"

	"github.com/guy5116/burrow/internal/wire"
)

// Mode selects strip (lossless, default) or paranoid (decode + re-encode).
type Mode uint8

// Modes.
const (
	ModeStrip Mode = iota
	ModeParanoid
	ModeRaw // a file sent byte for byte (PrepareFile)
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
	Ext      string // ModeRaw only: the extension offered to the peer

	orientation int  // EXIF orientation found by pass 1; above 1 means re-encode
	byContent   bool // the file's extension was not checked against its bytes
}

// KeepsMetadata names the photo and video formats Burrow cannot clean: HEIC,
// AVIF and other ISO media files (MP4, MOV, 3GP), TIFF and the camera RAW
// formats built on it, JPEG XL, Matroska and AVI. Sent as files, they would
// carry their location, capture time and camera model to the contact. It
// returns "" for anything else. head is the start of the file.
func KeepsMetadata(head []byte) string {
	has := func(off int, s string) bool { return len(head) >= off+len(s) && string(head[off:off+len(s)]) == s }
	switch {
	case has(4, "ftyp"):
		return "an HEIC, AVIF, MP4 or MOV file"
	case has(0, "II*\x00"), has(0, "MM\x00*"), has(0, "IIRO"), has(0, "IIU\x00"):
		return "a TIFF or camera RAW photo"
	case has(0, "\xff\x0a"), has(0, "\x00\x00\x00\x0cJXL \x0d\x0a\x87\x0a"):
		return "a JPEG XL image"
	case has(0, "\x1a\x45\xdf\xa3"):
		return "a Matroska or WebM video"
	case has(0, "RIFF") && has(8, "AVI "):
		return "an AVI video"
	}
	return ""
}

// FormatFile marks a transfer that is not an image: the bytes travel as they are.
const FormatFile Format = 0

// ErrEmpty means the file has no content to send.
var ErrEmpty = errBase("media: file is empty")

// FileExt returns the extension a file offer may carry: lower case, without
// the dot, a–z and 0–9 only, at most wire.MaxFileExt bytes. Anything else
// yields "" (the receiver then saves the file as .bin).
func FileExt(path string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if len(ext) > wire.MaxFileExt {
		return ""
	}
	for i := 0; i < len(ext); i++ {
		if c := ext[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return ext
}

// PrepareFile is pass 1 for a file that is not an image: it hashes the bytes
// as they are. Nothing inside the file is removed or changed.
func PrepareFile(ctx context.Context, path string, maxSize uint64) (Prepared, error) {
	limit := uint64(wire.MaxTransferSize)
	if maxSize > 0 {
		limit = min(limit, maxSize)
	}
	st, err := os.Stat(path)
	switch {
	case err != nil:
		return Prepared{}, err
	case !st.Mode().IsRegular():
		return Prepared{}, ErrUnsupported
	case st.Size() == 0:
		return Prepared{}, ErrEmpty
	case uint64(st.Size()) > limit: // #nosec G115 -- a size is not negative
		return Prepared{}, ErrTooLarge // refused before reading a byte
	}
	h, _ := blake2b.New256(nil)
	cw := &countWriter{w: h}
	if err := copyFile(ctx, cw, path, limit); err != nil {
		return Prepared{}, err
	}
	switch {
	case cw.n > limit: // it grew while we read
		return Prepared{}, ErrTooLarge
	case cw.n == 0: // it was emptied while we read
		return Prepared{}, ErrEmpty
	}
	p := Prepared{Format: FormatFile, Size: cw.n, Mode: ModeRaw, Path: path, Ext: FileExt(path)}
	copy(p.Hash[:], h.Sum(nil))
	return p, nil
}

// ctxReader fails with the context's error once the context is done, so a
// pass over a large file can be cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyFile copies at most limit+1 bytes of path to w, so that a file that
// keeps growing still ends and the caller can tell it went over.
func copyFile(ctx context.Context, w io.Writer, path string, limit uint64) error {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, io.LimitReader(ctxReader{ctx, f}, int64(min(limit, wire.MaxTransferSize))+1)) // #nosec G115 -- bounded by MaxTransferSize
	return err
}

// ProbeFile sniffs and gates a file without decoding pixels or holding the
// file in memory. The file's extension must agree with its bytes.
func ProbeFile(path string) (Info, error) { return probeFile(path, true) }

func probeFile(path string, checkExt bool) (Info, error) {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, SniffLen)
	n, _ := io.ReadFull(f, head)
	format, err := Sniff(head[:n])
	if err != nil {
		return Info{}, err
	}
	if checkExt && !extensionAgrees(path, format) {
		return Info{}, ErrMismatch
	}
	st, err := f.Stat()
	if err != nil {
		return Info{}, err
	}
	if st.Size() > int64(wire.MaxImageDecodeBytes) {
		return Info{}, ErrTooLarge
	}
	return probe(f)
}

// Prepare runs pass 1 over path: gates, strips (or re-encodes), hashes, and
// records size and format for the offer. Nothing is written to disk and, in
// strip mode without rotation, the file is never held in memory.
func Prepare(ctx context.Context, path string, mode Mode, maxSize uint64) (Prepared, error) {
	return prepare(ctx, path, mode, maxSize, false)
}

// PrepareByContent is Prepare for a file that was offered as a file and
// turned out to be an image: its bytes decide, its extension does not
// (a photo saved as "scan.jfif" or "photo.png.bak" is still stripped).
func PrepareByContent(ctx context.Context, path string, mode Mode, maxSize uint64) (Prepared, error) {
	return prepare(ctx, path, mode, maxSize, true)
}

func prepare(ctx context.Context, path string, mode Mode, maxSize uint64, byContent bool) (Prepared, error) {
	info, err := probeFile(path, !byContent)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Format: info.Format, Width: info.Width, Height: info.Height, Animated: info.Animated, Mode: mode, Path: path, byContent: byContent}
	h, _ := blake2b.New256(nil)
	cw := &countWriter{w: h}
	// The lossless strip also reveals the orientation. When the image has to
	// be re-encoded after all, its output is thrown away.
	if p.orientation, err = strip(ctx, path, info.Format, cw); err != nil {
		return Prepared{}, cancelled(ctx, err)
	}
	if p.reencodes() {
		h.Reset()
		cw.n = 0
		if p.Format, err = reencode(ctx, path, mode, p.orientation, cw); err != nil {
			return Prepared{}, cancelled(ctx, err)
		}
		if orientationSwaps(p.orientation) {
			p.Width, p.Height = info.Height, info.Width
		}
	}
	if maxSize > 0 && cw.n > maxSize {
		return Prepared{}, ErrTooLarge
	}
	p.Size = cw.n
	copy(p.Hash[:], h.Sum(nil))
	return p, nil
}

// cancelled returns the context's error when the context has ended: the
// parsers report a read that was cut short as a malformed file.
func cancelled(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return err
}

// reencodes reports whether the image is decoded and encoded again instead of
// being stripped losslessly.
func (p Prepared) reencodes() bool { return p.Mode == ModeParanoid || p.orientation > 1 }

// Stream runs pass 2: it produces the same bytes as pass 1 and hands them to
// sink in chunks of the shape the offer implies. The moment the output stops
// matching pass 1 (the file changed) it returns ErrDiverged; sink never sees
// a chunk the offer did not announce.
func Stream(ctx context.Context, p Prepared, sink func(index uint32, chunk []byte) error) error {
	h, _ := blake2b.New256(nil)
	ck := &chunker{sink: sink, size: p.Size}
	out := io.MultiWriter(h, ck)
	format, orientation := p.Format, p.orientation
	var err error
	switch {
	case p.Mode == ModeRaw:
		err = copyFile(ctx, out, p.Path, p.Size)
	case p.reencodes():
		format, err = reencode(ctx, p.Path, p.Mode, p.orientation, out)
	default:
		var info Info
		if info, err = probeFile(p.Path, !p.byContent); err == nil {
			format = info.Format
			orientation, err = strip(ctx, p.Path, info.Format, out)
		}
	}
	if err != nil {
		return cancelled(ctx, err)
	}
	if err := ck.flush(); err != nil {
		return err
	}
	if format != p.Format || orientation != p.orientation || subtle.ConstantTimeCompare(h.Sum(nil), p.Hash[:]) != 1 {
		return ErrDiverged
	}
	return nil
}

// ErrMismatch means the file's extension claims a different format than its bytes.
var ErrMismatch = errBase("media: file extension does not match the image data")

// extensionAgrees reports whether path's extension (if any) names the sniffed
// format. A file without an extension cannot disagree.
func extensionAgrees(path string, f Format) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case "":
		return true
	case ".png":
		return f == wire.FormatPNG
	case ".jpg", ".jpeg":
		return f == wire.FormatJPEG
	case ".webp":
		return f == wire.FormatWebP
	case ".gif":
		return f == wire.FormatGIF
	}
	return false
}

// NewHasher returns the BLAKE2b-256 hasher used for transfer integrity, so
// core never imports x/crypto directly.
func NewHasher() hash.Hash {
	h, _ := blake2b.New256(nil)
	return h
}

// ErrDiverged means the second stripping pass produced different bytes.
var ErrDiverged = errBase("media: file changed between passes")

type errBase string

func (e errBase) Error() string { return string(e) }

// strip writes the image at path to w without its metadata, losslessly, and
// returns the EXIF orientation it found (0 if none).
func strip(ctx context.Context, path string, format Format, w io.Writer) (orientation int, err error) {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	r := ctxReader{ctx, f}
	switch format {
	case wire.FormatJPEG:
		return stripJPEG(w, r)
	case wire.FormatPNG:
		return stripPNG(w, r)
	case wire.FormatWebP:
		return stripWebP(w, ctxSeeker{r, f})
	case wire.FormatGIF:
		return 0, stripGIF(w, r)
	}
	return 0, ErrUnsupported
}

// ctxSeeker is a ctxReader that can rewind (stripWebP reads its input twice).
type ctxSeeker struct {
	ctxReader
	io.Seeker
}

func orientationSwaps(o int) bool { return o >= 5 && o <= 8 }

// reencode decodes the image at path, applies the orientation, and encodes:
// PNG → PNG, JPEG → JPEG q92, WebP → PNG, GIF → GIF (animated keeps its frames
// and delays, nothing else). Paranoid mode always produces PNG except for
// animated GIFs. The bytes that are decoded are the bytes that were gated,
// and one decode slot is held from the decode to the end of the encode.
func reencode(ctx context.Context, path string, mode Mode, orientation int, w io.Writer) (Format, error) {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return 0, err
	}
	data, err := readAll(ctxReader{ctx, f}, int64(wire.MaxImageDecodeBytes))
	_ = f.Close()
	if err != nil {
		return 0, err
	}
	info, err := Probe(data)
	if err != nil {
		return 0, err
	}
	if err := acquire(ctx); err != nil {
		return 0, err
	}
	defer release()
	if info.Format == wire.FormatGIF && info.Animated {
		g, err := gif.DecodeAll(bytes.NewReader(data)) // bounded by the pre-scan in Probe
		if err != nil {
			return 0, ErrCorrupt
		}
		out := &gif.GIF{Image: g.Image, Delay: g.Delay, LoopCount: g.LoopCount, Disposal: g.Disposal, Config: g.Config}
		return wire.FormatGIF, gif.EncodeAll(w, out)
	}
	img, err := decode(data, info.Format)
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

// chunker cuts a stream of exactly size bytes into ChunkData-sized chunks
// (the last one shorter). Any other length is ErrDiverged, reported before a
// chunk of the wrong shape reaches the sink.
type chunker struct {
	sink  func(index uint32, chunk []byte) error
	size  uint64
	buf   bytes.Buffer
	index uint32
	total uint64
}

func (c *chunker) Write(p []byte) (int, error) {
	n := len(p)
	c.total += uint64(n) // #nosec G115 -- n ≥ 0
	if c.total > c.size {
		return 0, ErrDiverged
	}
	for len(p) > 0 {
		take := min(wire.ChunkData-c.buf.Len(), len(p))
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

// flush emits the last, shorter chunk once the stream has ended.
func (c *chunker) flush() error {
	switch {
	case c.total != c.size:
		return ErrDiverged
	case c.buf.Len() == 0:
		return nil
	}
	return c.emit()
}
