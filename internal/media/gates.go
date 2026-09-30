// Package media gates, sniffs, strips and safely decodes images (CLAUDE.md §6).
// Nothing here trusts a sender's declared format: every decision starts from
// the magic bytes, and no decode happens before the size gates pass.
package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"

	"github.com/guy5116/burrow/internal/wire"
)

// Format is the wire image format (wire.FormatPNG …).
type Format = uint8

// Errors.
var (
	ErrUnsupported   = errors.New("media: unsupported image format")
	ErrTooLarge      = errors.New("media: image exceeds size limits")
	ErrCorrupt       = errors.New("media: malformed image")
	ErrAnimated      = errors.New("media: animated WebP is not supported")
	ErrTooManyFrames = errors.New("media: too many GIF frames")
)

// SniffLen is how many leading bytes Sniff needs.
const SniffLen = 16

// Sniff identifies the format from magic bytes.
func Sniff(head []byte) (Format, error) {
	switch {
	case len(head) >= 8 && bytes.Equal(head[:8], []byte("\x89PNG\r\n\x1a\n")):
		return wire.FormatPNG, nil
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return wire.FormatJPEG, nil
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return wire.FormatWebP, nil
	case len(head) >= 6 && (bytes.Equal(head[:6], []byte("GIF87a")) || bytes.Equal(head[:6], []byte("GIF89a"))):
		return wire.FormatGIF, nil
	}
	return 0, ErrUnsupported
}

// Ext returns the file extension for a sniffed format.
func Ext(f Format) string {
	switch f {
	case wire.FormatPNG:
		return "png"
	case wire.FormatJPEG:
		return "jpg"
	case wire.FormatWebP:
		return "webp"
	case wire.FormatGIF:
		return "gif"
	}
	return "bin"
}

// Info is what the gates learn without decoding pixels.
type Info struct {
	Format   Format
	Width    int
	Height   int
	Animated bool // GIF with > 1 frame
	Frames   int
}

// CheckDimensions applies the §6.1 pixel limits to declared dimensions.
func CheckDimensions(w, h int) error {
	if w <= 0 || h <= 0 || w > wire.MaxImageSide || h > wire.MaxImageSide || w*h > wire.MaxImagePixels {
		return ErrTooLarge
	}
	return nil
}

// maxJPEGScans bounds the scans of a progressive JPEG. Every scan is a pass
// over the whole image, so a file with thousands of tiny scans costs seconds
// of CPU per decode. Encoders write about ten.
const maxJPEGScans = 100

// jpegScanBytes returns what image/jpeg allocates on top of the image while
// it decodes a progressive JPEG: one block of 64 int32 coefficients per 8×8
// block of every component, kept until the last scan. For a CMYK image that
// is 16 bytes per pixel, four times the image. A baseline JPEG needs nothing
// extra, and only its headers up to the frame header are read. A progressive
// one is read to the end, to count its scans.
func jpegScanBytes(r io.Reader, w, h int) (int, error) {
	br := bufio.NewReader(r)
	if b, err := br.Peek(2); err != nil || b[0] != 0xFF || b[1] != 0xD8 {
		return 0, ErrCorrupt
	}
	_, _ = br.Discard(2)
	extra, scans := -1, 0 // extra stays -1 until the frame header is found
	for {
		m, err := nextMarker(br)
		if err != nil {
			return 0, err
		}
		switch {
		case m == 0xD9: // end of image
			if extra < 0 {
				return 0, ErrCorrupt
			}
			return extra, nil
		case m >= 0xD0 && m <= 0xD8 || m == 0x01:
			continue // markers without a length
		}
		var lb [2]byte
		if _, err := io.ReadFull(br, lb[:]); err != nil {
			return 0, ErrCorrupt
		}
		n := int(lb[0])<<8 | int(lb[1]) - 2
		switch {
		case n < 0:
			return 0, ErrCorrupt
		case m == 0xC0 || m == 0xC1: // baseline, extended sequential
			if extra < 0 {
				return 0, nil
			}
		case m == 0xC2 && extra < 0: // progressive
			if extra, err = progressiveBytes(br, n, w, h); err != nil {
				return 0, err
			}
			continue
		case m == 0xDA: // a scan: its header, then entropy-coded data up to the next marker
			if scans++; extra < 0 || scans > maxJPEGScans {
				return 0, map[bool]error{true: ErrCorrupt, false: ErrTooLarge}[extra < 0]
			}
			if _, err := br.Discard(n); err != nil {
				return 0, ErrCorrupt
			}
			if err := skipEntropyData(br); err != nil {
				return 0, err
			}
			continue
		}
		if _, err := br.Discard(n); err != nil {
			return 0, ErrCorrupt
		}
	}
}

// nextMarker reads fill bytes and returns the next marker code.
func nextMarker(br *bufio.Reader) (byte, error) {
	b, err := br.ReadByte()
	if err != nil || b != 0xFF {
		return 0, ErrCorrupt
	}
	for b == 0xFF {
		if b, err = br.ReadByte(); err != nil {
			return 0, ErrCorrupt
		}
	}
	return b, nil
}

// skipEntropyData reads up to the next marker that is not part of the scan
// (0xFF 0x00 stuffing and restart markers are) and leaves it unread.
func skipEntropyData(br *bufio.Reader) error {
	for {
		b, err := br.Peek(2)
		if err != nil {
			return ErrCorrupt
		}
		switch {
		case b[0] != 0xFF:
			_, _ = br.Discard(1)
		case b[1] == 0xFF: // a fill byte before a marker
			_, _ = br.Discard(1)
		case b[1] == 0x00 || b[1] >= 0xD0 && b[1] <= 0xD7:
			_, _ = br.Discard(2)
		default:
			return nil
		}
	}
}

// progressiveBytes reads the frame header of a progressive JPEG (n bytes)
// and returns the coefficient memory image/jpeg keeps for it: per component
// its share of 8×8 blocks, 256 bytes each.
func progressiveBytes(br *bufio.Reader, n, w, h int) (int, error) {
	if n < 6 {
		return 0, ErrCorrupt
	}
	sof := make([]byte, n) // at most 64 KiB: the length field is 16 bits
	if _, err := io.ReadFull(br, sof); err != nil || len(sof) < 6+3*int(sof[5]) {
		return 0, ErrCorrupt
	}
	comps := sof[6 : 6+3*int(sof[5])]
	hmax, vmax := 1, 1
	for i := 0; i < len(comps); i += 3 {
		hmax, vmax = max(hmax, int(comps[i+1]>>4)), max(vmax, int(comps[i+1]&15))
	}
	mxx, myy := (w+8*hmax-1)/(8*hmax), (h+8*vmax-1)/(8*vmax)
	blocks := 0
	for i := 0; i < len(comps); i += 3 {
		blocks += mxx * myy * int(comps[i+1]>>4) * int(comps[i+1]&15)
	}
	return blocks * 64 * 4, nil
}

func bytesPerPixel(m color.Model) int {
	switch m {
	case color.GrayModel, color.AlphaModel:
		return 1
	case color.Gray16Model, color.Alpha16Model:
		return 2
	case color.YCbCrModel:
		return 3
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	return 4
}

// Probe sniffs and gates an image without decoding pixels. For GIF it
// pre-scans the block structure to count frames and sum frame areas.
func Probe(data []byte) (Info, error) { return probe(bytes.NewReader(data)) }

// probe is Probe over a file or buffer. It reads headers and, for GIF, walks
// the block structure; the image is never held in memory.
func probe(r io.ReadSeeker) (Info, error) {
	rewind := func() (io.Reader, error) {
		_, err := r.Seek(0, io.SeekStart)
		return bufio.NewReader(r), err
	}
	rd, err := rewind()
	if err != nil {
		return Info{}, err
	}
	head := make([]byte, SniffLen)
	n, _ := io.ReadFull(rd, head)
	f, err := Sniff(head[:n])
	if err != nil {
		return Info{}, err
	}
	if rd, err = rewind(); err != nil {
		return Info{}, err
	}
	var cfg image.Config
	switch f {
	case wire.FormatPNG:
		cfg, err = png.DecodeConfig(rd)
	case wire.FormatJPEG:
		cfg, err = jpeg.DecodeConfig(rd)
	case wire.FormatWebP:
		cfg, err = webp.DecodeConfig(rd)
	case wire.FormatGIF:
		cfg, err = gif.DecodeConfig(rd)
	}
	if err != nil {
		return Info{}, ErrCorrupt
	}
	if err := CheckDimensions(cfg.Width, cfg.Height); err != nil {
		return Info{}, err
	}
	need := cfg.Width * cfg.Height * bytesPerPixel(cfg.ColorModel)
	switch f {
	case wire.FormatJPEG:
		if rd, err = rewind(); err != nil {
			return Info{}, err
		}
		extra, err := jpegScanBytes(rd, cfg.Width, cfg.Height)
		if err != nil {
			return Info{}, err
		}
		need += extra
	case wire.FormatPNG:
		// image/png decodes an interlaced image pass by pass into images of
		// their own before it assembles the result: count it twice.
		if rd, err = rewind(); err != nil {
			return Info{}, err
		}
		var ihdr [8 + 8 + 13]byte
		if _, err := io.ReadFull(rd, ihdr[:]); err != nil {
			return Info{}, ErrCorrupt
		}
		if ihdr[8+8+12] == 1 {
			need *= 2
		}
	}
	if need > wire.MaxImageDecodeBytes {
		return Info{}, ErrTooLarge
	}
	info := Info{Format: f, Width: cfg.Width, Height: cfg.Height, Frames: 1}
	if f != wire.FormatGIF && f != wire.FormatWebP {
		return info, nil
	}
	if rd, err = rewind(); err != nil {
		return Info{}, err
	}
	if f == wire.FormatWebP {
		if animated, err := webpAnimated(rd); err != nil {
			return Info{}, err
		} else if animated {
			return Info{}, ErrAnimated
		}
		return info, nil
	}
	frames, _, err := gifPrescan(rd)
	if err != nil {
		return Info{}, err
	}
	info.Frames, info.Animated = frames, frames > 1
	return info, nil
}

// decodeSem allows two full-size decodes at a time process-wide.
var decodeSem = make(chan struct{}, wire.MaxConcurrentDecode)

// acquire takes a decode slot, waiting while two decodes are running.
func acquire(ctx context.Context) error {
	select {
	case decodeSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func release() { <-decodeSem }

// Decode gates then decodes with the stdlib decoders only (x/image/webp for
// WebP). Animated GIFs return the first frame. It waits for a decode slot.
func Decode(ctx context.Context, data []byte) (image.Image, Info, error) {
	info, err := Probe(data)
	if err != nil {
		return nil, info, err
	}
	if err := acquire(ctx); err != nil {
		return nil, info, err
	}
	defer release()
	img, err := decode(data, info.Format)
	return img, info, err
}

// decode runs the decoder for a format. The caller has gated data with Probe
// and holds a decode slot.
func decode(data []byte, f Format) (img image.Image, err error) {
	r := bytes.NewReader(data)
	switch f {
	case wire.FormatPNG:
		img, err = png.Decode(r)
	case wire.FormatJPEG:
		img, err = jpeg.Decode(r)
	case wire.FormatWebP:
		img, err = webp.Decode(r)
	case wire.FormatGIF:
		img, err = gif.Decode(r)
	}
	if err != nil {
		return nil, ErrCorrupt
	}
	return img, nil
}

// Thumbnail scales img to fit within max×max (never upscales).
func Thumbnail(img image.Image, max int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return img
	}
	if w >= h {
		h = h * max / w
		w = max
	} else {
		w = w * max / h
		h = max
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// readAll bounds a reader to the image size limit.
func readAll(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrTooLarge
	}
	return b, nil
}
