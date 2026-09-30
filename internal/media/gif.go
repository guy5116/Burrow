package media

import (
	"bufio"
	"io"

	"github.com/guy5116/burrow/internal/wire"
)

// gifPrescan walks the block structure without decoding LZW data and returns
// the frame count and the sum of frame areas. It stops with ErrTooLarge as
// soon as either passes its limit.
func gifPrescan(rd io.Reader) (frames, area int, err error) {
	r := bufio.NewReader(rd)
	if _, err := skipGIFHeader(r, io.Discard); err != nil {
		return 0, 0, err
	}
	for {
		b, err := r.ReadByte()
		if err != nil {
			return 0, 0, ErrCorrupt
		}
		switch b {
		case 0x3B:
			return frames, area, nil
		case 0x21:
			label, err := r.ReadByte()
			if err != nil {
				return 0, 0, ErrCorrupt
			}
			if err := skipExtension(r, label, io.Discard); err != nil {
				return 0, 0, err
			}
		case 0x2C:
			var d [9]byte
			if _, err := io.ReadFull(r, d[:]); err != nil {
				return 0, 0, ErrCorrupt
			}
			w, h := int(d[4])|int(d[5])<<8, int(d[6])|int(d[7])<<8
			frames++
			area += w * h
			if frames > wire.MaxGIFFrames || area > wire.MaxImagePixels {
				return 0, 0, ErrTooLarge
			}
			if d[8]&0x80 != 0 {
				if _, err := r.Discard(3 << (d[8]&7 + 1)); err != nil {
					return 0, 0, ErrCorrupt
				}
			}
			if _, err := r.ReadByte(); err != nil { // LZW min code size
				return 0, 0, ErrCorrupt
			}
			if err := skipSubBlocks(r, io.Discard); err != nil {
				return 0, 0, err
			}
		default:
			return 0, 0, ErrCorrupt
		}
	}
}

// skipGIFHeader copies (or discards) the header, logical screen descriptor
// and global color table.
func skipGIFHeader(r *bufio.Reader, w io.Writer) (int, error) {
	hdr := make([]byte, 13)
	if _, err := io.ReadFull(r, hdr); err != nil || (string(hdr[:6]) != "GIF87a" && string(hdr[:6]) != "GIF89a") {
		return 0, ErrCorrupt
	}
	if _, err := w.Write(hdr); err != nil {
		return 0, err
	}
	n := 13
	if hdr[10]&0x80 != 0 {
		gct := make([]byte, 3<<(hdr[10]&7+1))
		if _, err := io.ReadFull(r, gct); err != nil {
			return 0, ErrCorrupt
		}
		if _, err := w.Write(gct); err != nil {
			return 0, err
		}
		n += len(gct)
	}
	return n, nil
}

// skipExtension walks the extension with the given label, copying its bytes
// to w, and ends exactly where image/gif ends it. Where the two could
// disagree, the file is refused: a frame the pre-scan does not see would
// escape the frame and area limits. image/gif reads a plain-text extension
// as 13 fixed bytes and a graphic control block as 6, and refuses any label
// it does not know.
func skipExtension(r *bufio.Reader, label byte, w io.Writer) error {
	switch label {
	case 0x01: // plain text: the fixed part is one sub-block only when it says 12
		if b, err := r.Peek(1); err != nil || b[0] != 12 {
			return ErrCorrupt
		}
	case 0xF9: // graphic control: exactly one 4-byte sub-block and the terminator
		if b, err := r.Peek(6); err != nil || b[0] != 4 || b[5] != 0 {
			return ErrCorrupt
		}
	case 0xFF: // application: an empty identifier would be read differently
		if b, err := r.Peek(1); err != nil || b[0] == 0 {
			return ErrCorrupt
		}
	case 0xFE: // comment: sub-blocks only
	default:
		return ErrCorrupt
	}
	return skipSubBlocks(r, w)
}

// skipSubBlocks copies data sub-blocks up to and including the terminator.
func skipSubBlocks(r *bufio.Reader, w io.Writer) error {
	var buf [1 + 255]byte // length byte + the largest sub-block
	for {
		n, err := r.ReadByte()
		if err != nil {
			return ErrCorrupt
		}
		buf[0] = n
		if _, err := io.ReadFull(r, buf[1:1+int(n)]); err != nil {
			return ErrCorrupt
		}
		if _, err := w.Write(buf[:1+int(n)]); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// head keeps the first bytes written to it and counts all of them, so an
// extension of any length can be examined without being held in memory.
type head struct {
	b []byte
	n int
}

func (h *head) Write(p []byte) (int, error) {
	h.n += len(p)
	h.b = append(h.b, p[:min(len(p), cap(h.b)-len(h.b))]...)
	return len(p), nil
}

// gifKeepExtension says whether an extension survives: a graphic control
// block, or the NETSCAPE2.0 loop count, each only in its one legal shape.
// Comments, plain text, other application data and anything hiding under
// those two labels are dropped.
func gifKeepExtension(label byte, body []byte) bool {
	switch {
	case label == 0xF9 && len(body) == 6: // one 4-byte sub-block and the terminator
		b := [6]byte(body)
		return b[0] == 4 && b[5] == 0
	case label == 0xFF && len(body) == 17: // "NETSCAPE2.0", sub-block {1, loop count u16}, terminator
		b := [17]byte(body)
		return b[0] == 11 && string(b[1:12]) == "NETSCAPE2.0" && b[12] == 3 && b[13] == 1 && b[16] == 0
	}
	return false
}

// stripGIF drops comment, plain-text and application extensions except
// NETSCAPE2.0 looping, and anything after the trailer.
func stripGIF(w io.Writer, r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	if _, err := skipGIFHeader(br, bw); err != nil {
		return err
	}
	for {
		b, err := br.ReadByte()
		if err != nil {
			return ErrCorrupt
		}
		switch b {
		case 0x3B:
			if err := bw.WriteByte(0x3B); err != nil {
				return err
			}
			return bw.Flush()
		case 0x21:
			label, err := br.ReadByte()
			if err != nil {
				return ErrCorrupt
			}
			ext := head{b: make([]byte, 0, 32)}
			if err := skipExtension(br, label, &ext); err != nil {
				return err
			}
			if ext.n == len(ext.b) && gifKeepExtension(label, ext.b) {
				if _, err := bw.Write([]byte{0x21, label}); err != nil {
					return err
				}
				if _, err := bw.Write(ext.b); err != nil {
					return err
				}
			}
		case 0x2C:
			var d [9]byte
			if _, err := io.ReadFull(br, d[:]); err != nil {
				return ErrCorrupt
			}
			if _, err := bw.Write(append([]byte{0x2C}, d[:]...)); err != nil {
				return err
			}
			if d[8]&0x80 != 0 {
				lct := make([]byte, 3<<(d[8]&7+1))
				if _, err := io.ReadFull(br, lct); err != nil {
					return ErrCorrupt
				}
				if _, err := bw.Write(lct); err != nil {
					return err
				}
			}
			mcs, err := br.ReadByte()
			if err != nil {
				return ErrCorrupt
			}
			if err := bw.WriteByte(mcs); err != nil {
				return err
			}
			if err := skipSubBlocks(br, bw); err != nil {
				return err
			}
		default:
			return ErrCorrupt
		}
	}
}
