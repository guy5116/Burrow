package media

import (
	"bufio"
	"encoding/binary"
	"io"
)

// JPEG markers.
const (
	mSOI   = 0xD8
	mEOI   = 0xD9
	mSOS   = 0xDA
	mDQT   = 0xDB
	mDHT   = 0xC4
	mDAC   = 0xCC
	mDRI   = 0xDD
	mAPP0  = 0xE0
	mAPP1  = 0xE1
	mAPP14 = 0xEE
	mCOM   = 0xFE
)

// minimal JFIF APP0: length 16, "JFIF\0", 1.01, no units, 1×1, no thumbnail.
var minimalJFIF = []byte{0xFF, mAPP0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0, 1, 1, 0, 0, 1, 0, 1, 0, 0}

func isSOF(m byte) bool {
	return m >= 0xC0 && m <= 0xCF && m != mDHT && m != 0xC8 && m != mDAC
}

// jpegKeep says whether a segment survives strip mode.
func jpegKeep(m byte) bool {
	return isSOF(m) || m == mDQT || m == mDHT || m == mDAC || m == mDRI || m == mSOS || m == mAPP14
}

// stripJPEG streams r to w keeping only structural segments. It returns the
// EXIF orientation seen (0 if none) so the caller can decide to re-encode.
func stripJPEG(w io.Writer, r io.Reader) (orientation int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	var b [2]byte
	if _, err := io.ReadFull(br, b[:]); err != nil || b[0] != 0xFF || b[1] != mSOI {
		return 0, ErrCorrupt
	}
	if _, err := bw.Write(b[:]); err != nil {
		return 0, err
	}
	jfifDone := false
	afterScan := false // copyScan already consumed the 0xFF of the next marker
	for {
		// Next marker: skip fill bytes.
		m := byte(0xFF)
		if !afterScan {
			if m, err = br.ReadByte(); err != nil {
				return 0, ErrCorrupt
			}
			if m != 0xFF {
				return 0, ErrCorrupt
			}
		}
		afterScan = false
		for m == 0xFF {
			if m, err = br.ReadByte(); err != nil {
				return 0, ErrCorrupt
			}
		}
		switch {
		case m == mEOI:
			if _, err := bw.Write([]byte{0xFF, mEOI}); err != nil {
				return 0, err
			}
			return orientation, bw.Flush() // anything after EOI is dropped
		case m == 0x00 || (m >= 0xD0 && m <= 0xD7):
			return 0, ErrCorrupt // stray stuffing/RST outside a scan
		}
		var lb [2]byte
		if _, err := io.ReadFull(br, lb[:]); err != nil {
			return 0, ErrCorrupt
		}
		n := int(binary.BigEndian.Uint16(lb[:]))
		if n < 2 {
			return 0, ErrCorrupt
		}
		payload := make([]byte, n-2)
		if _, err := io.ReadFull(br, payload); err != nil {
			return 0, ErrCorrupt
		}
		switch {
		case m == mAPP0:
			if !jfifDone && len(payload) >= 5 && string(payload[:5]) == "JFIF\x00" {
				if _, err := bw.Write(minimalJFIF); err != nil {
					return 0, err
				}
				jfifDone = true
			}
		case m == mAPP1:
			if len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" && orientation == 0 {
				orientation = exifOrientation(payload[6:])
			}
		case jpegKeep(m):
			if _, err := bw.Write([]byte{0xFF, m, lb[0], lb[1]}); err != nil {
				return 0, err
			}
			if _, err := bw.Write(payload); err != nil {
				return 0, err
			}
			if m == mSOS {
				if err := copyScan(bw, br); err != nil {
					return 0, err
				}
				afterScan = true
			}
		}
	}
}

// copyScan copies entropy-coded data up to (not including) the next real
// marker. 0xFF00 stuffing and RSTn markers belong to the scan.
func copyScan(bw *bufio.Writer, br *bufio.Reader) error {
	for {
		c, err := br.ReadByte()
		if err != nil {
			return ErrCorrupt
		}
		if c != 0xFF {
			if err := bw.WriteByte(c); err != nil {
				return err
			}
			continue
		}
		next, err := br.Peek(1)
		if err != nil {
			return ErrCorrupt
		}
		if next[0] == 0x00 || (next[0] >= 0xD0 && next[0] <= 0xD7) {
			b := next[0]
			_, _ = br.ReadByte()
			if err := bw.WriteByte(0xFF); err != nil {
				return err
			}
			if err := bw.WriteByte(b); err != nil {
				return err
			}
			continue
		}
		if next[0] == 0xFF { // fill byte; drop this 0xFF and continue
			continue
		}
		// A real marker: the 0xFF is consumed; the segment loop reads the marker byte next.
		return nil
	}
}
