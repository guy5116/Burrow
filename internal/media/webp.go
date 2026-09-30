package media

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
)

// VP8X feature flags.
const (
	vp8xICC   = 0x20
	vp8xAlpha = 0x10
	vp8xEXIF  = 0x08
	vp8xXMP   = 0x04
	vp8xAnim  = 0x02
	vp8xLen   = 10 // flags, 3 reserved bytes, canvas width and height (3 bytes each)
)

// webpKeep names the chunks a still image consists of. Everything else is
// dropped: known metadata (EXIF, XMP, ICCP) and any chunk we do not know.
func webpKeep(fourcc string) bool {
	switch fourcc {
	case "VP8 ", "VP8L", "VP8X", "ALPH":
		return true
	}
	return false
}

// webpChunk is one chunk header: its eight raw bytes, the payload length and
// the payload length including the pad byte of an odd-sized chunk.
type webpChunk struct {
	raw    [8]byte
	fourcc string
	n      int64
	padded int64
}

// webpWalk checks the RIFF header and calls visit once per chunk, in order.
// visit must consume exactly c.padded bytes from br.
func webpWalk(br *bufio.Reader, visit func(c webpChunk) error) error {
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(br, hdr); err != nil || string(hdr[:4]) != "RIFF" || string(hdr[8:12]) != "WEBP" {
		return ErrCorrupt
	}
	remaining := int64(binary.LittleEndian.Uint32(hdr[4:])) - 4
	if remaining < 0 {
		return ErrCorrupt
	}
	for remaining > 0 {
		var c webpChunk
		if _, err := io.ReadFull(br, c.raw[:]); err != nil {
			return ErrCorrupt
		}
		c.fourcc = string(c.raw[:4])
		c.n = int64(binary.LittleEndian.Uint32(c.raw[4:]))
		c.padded = c.n + c.n&1
		if c.padded+8 > remaining {
			return ErrCorrupt
		}
		remaining -= 8 + c.padded
		if err := visit(c); err != nil {
			return err
		}
	}
	return nil
}

// webpAnimated reports whether the file declares animation. Only the
// extended format (first chunk VP8X) can; the flag is in its first byte.
func webpAnimated(r io.Reader) (animated bool, err error) {
	br := bufio.NewReader(r)
	errStop := errBase("stop")
	err = webpWalk(br, func(c webpChunk) error {
		if c.fourcc == "VP8X" {
			flags, err := br.ReadByte()
			if err != nil || c.n < 10 {
				return ErrCorrupt
			}
			animated = flags&vp8xAnim != 0
		}
		return errStop // the first chunk decides
	})
	if errors.Is(err, errStop) {
		err = nil
	}
	return animated, err
}

// stripWebP keeps the image chunks (webpKeep), clears the metadata bits in
// VP8X and rewrites the RIFF size. Animated files are rejected. It reads r
// twice, first to learn the output size the RIFF header must state, so the
// file is never held in memory. It returns the EXIF orientation (0 if none).
func stripWebP(w io.Writer, r io.ReadSeeker) (orientation int, err error) {
	buf := make([]byte, copyBuf)
	br := bufio.NewReaderSize(r, 64<<10)
	size := int64(4) // "WEBP"
	err = webpWalk(br, func(c webpChunk) error {
		if webpKeep(c.fourcc) {
			size += 8 + c.padded
		}
		switch c.fourcc {
		case "ANIM", "ANMF":
			return ErrAnimated
		case "VP8X":
			flags, err := br.ReadByte()
			switch {
			case err != nil || c.n != vp8xLen:
				return ErrCorrupt
			case flags&vp8xAnim != 0:
				return ErrAnimated
			}
			return copyN(io.Discard, br, c.padded-1, buf)
		case "EXIF":
			t, err := readHead(br, c.padded, io.Discard, buf)
			if err != nil || orientation != 0 {
				return err
			}
			t = t[:min(int64(len(t)), c.n)]
			if len(t) >= 6 && string(t[:6]) == "Exif\x00\x00" {
				t = t[6:]
			}
			orientation = exifOrientation(t)
			return nil
		}
		return copyN(io.Discard, br, c.padded, buf)
	})
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	br.Reset(r)
	bw := bufio.NewWriterSize(w, 64<<10)
	var riff [12]byte
	copy(riff[:4], "RIFF")
	binary.LittleEndian.PutUint32(riff[4:], uint32(size)) // #nosec G115 -- at most the input's own 32-bit RIFF size
	copy(riff[8:], "WEBP")
	if _, err := bw.Write(riff[:]); err != nil {
		return 0, err
	}
	err = webpWalk(br, func(c webpChunk) error {
		if !webpKeep(c.fourcc) {
			return copyN(io.Discard, br, c.padded, buf)
		}
		if _, err := bw.Write(c.raw[:]); err != nil {
			return err
		}
		if c.fourcc == "VP8X" {
			// Of the flags only "has alpha" survives (metadata is gone, and
			// animation was refused); the reserved bytes become zero.
			var x [vp8xLen]byte
			if _, err := io.ReadFull(br, x[:]); err != nil {
				return ErrCorrupt
			}
			x[0] &= vp8xAlpha
			x[1], x[2], x[3] = 0, 0, 0
			_, err := bw.Write(x[:])
			return err
		}
		if err := copyN(bw, br, c.n, buf); err != nil {
			return err
		}
		if c.padded > c.n { // the pad byte of an odd-sized chunk is written as zero
			if _, err := br.Discard(1); err != nil {
				return ErrCorrupt
			}
			return bw.WriteByte(0)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return orientation, bw.Flush()
}
