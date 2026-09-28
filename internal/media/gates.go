// Package media gates, sniffs, strips and safely decodes images (CLAUDE.md §6).
// Nothing here trusts a sender's declared format: every decision starts from
// the magic bytes, and no decode happens before the size gates pass.
package media

import (
	"bytes"
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
func Probe(data []byte) (Info, error) {
	f, err := Sniff(data)
	if err != nil {
		return Info{}, err
	}
	var cfg image.Config
	switch f {
	case wire.FormatPNG:
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case wire.FormatJPEG:
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case wire.FormatWebP:
		cfg, err = webp.DecodeConfig(bytes.NewReader(data))
	case wire.FormatGIF:
		cfg, err = gif.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil {
		return Info{}, ErrCorrupt
	}
	if err := CheckDimensions(cfg.Width, cfg.Height); err != nil {
		return Info{}, err
	}
	if cfg.ColorModel != nil && cfg.Width*cfg.Height*bytesPerPixel(cfg.ColorModel) > wire.MaxImageDecodeBytes {
		return Info{}, ErrTooLarge
	}
	info := Info{Format: f, Width: cfg.Width, Height: cfg.Height, Frames: 1}
	switch f {
	case wire.FormatGIF:
		frames, area, err := gifPrescan(data)
		if err != nil {
			return Info{}, err
		}
		if frames > wire.MaxGIFFrames || area > wire.MaxImagePixels {
			return Info{}, ErrTooLarge
		}
		info.Frames, info.Animated = frames, frames > 1
	case wire.FormatWebP:
		if webpAnimated(data) {
			return Info{}, ErrAnimated
		}
	}
	return info, nil
}

// decodeSem allows two full-size decodes at a time process-wide.
var decodeSem = make(chan struct{}, wire.MaxConcurrentDecode)

// Decode gates then decodes with the stdlib decoders only (x/image/webp for
// WebP). Animated GIFs return the first frame. It blocks while two decodes
// are already running.
func Decode(data []byte) (image.Image, Info, error) {
	info, err := Probe(data)
	if err != nil {
		return nil, info, err
	}
	decodeSem <- struct{}{}
	defer func() { <-decodeSem }()
	var img image.Image
	r := bytes.NewReader(data)
	switch info.Format {
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
		return nil, info, ErrCorrupt
	}
	return img, info, nil
}

// DecodeGIF gates then decodes every frame (bounded by the pre-scan).
func DecodeGIF(data []byte) (*gif.GIF, error) {
	info, err := Probe(data)
	if err != nil {
		return nil, err
	}
	if info.Format != wire.FormatGIF {
		return nil, ErrUnsupported
	}
	decodeSem <- struct{}{}
	defer func() { <-decodeSem }()
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil {
		return nil, ErrCorrupt
	}
	return g, nil
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
