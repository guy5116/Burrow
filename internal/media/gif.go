package media

import (
	"bufio"
	"bytes"
	"io"
)

// gifPrescan walks the block structure without decoding LZW data and returns
// the frame count and the sum of frame areas.
func gifPrescan(data []byte) (frames, area int, err error) {
	r := bufio.NewReader(bytes.NewReader(data))
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
			if _, err := r.ReadByte(); err != nil {
				return 0, 0, ErrCorrupt
			}
			if err := skipSubBlocks(r, io.Discard); err != nil {
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
			if frames > 4096 || area > 1<<31 {
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

// skipSubBlocks copies data sub-blocks up to and including the terminator.
func skipSubBlocks(r *bufio.Reader, w io.Writer) error {
	for {
		n, err := r.ReadByte()
		if err != nil {
			return ErrCorrupt
		}
		if _, err := w.Write([]byte{n}); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return ErrCorrupt
		}
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
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
			var buf bytes.Buffer
			if err := skipSubBlocks(br, &buf); err != nil {
				return err
			}
			keep := label == 0xF9 // graphic control
			if label == 0xFF && buf.Len() >= 12 && bytes.HasPrefix(buf.Bytes()[1:], []byte("NETSCAPE2.0")) {
				keep = true
			}
			if keep {
				if _, err := bw.Write([]byte{0x21, label}); err != nil {
					return err
				}
				if _, err := bw.Write(buf.Bytes()); err != nil {
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
