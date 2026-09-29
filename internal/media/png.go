package media

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
)

var pngSig = []byte("\x89PNG\r\n\x1a\n")

const (
	maxPNGChunk = 1<<31 - 1 // the format's own limit
	maxEXIF     = 64 << 10  // bytes of an EXIF block examined for the orientation
	copyBuf     = 32 << 10
)

// pngKeep says whether a chunk of type t and length n survives strip mode.
// gAMA and sRGB have one legal length each; under any other they could carry
// arbitrary bytes, so they are dropped.
func pngKeep(t string, n uint32) bool {
	switch t {
	case "IHDR", "PLTE", "tRNS", "IDAT", "IEND":
		return true
	case "gAMA":
		return n == 4
	case "sRGB":
		return n == 1
	}
	return false
}

// copyN moves exactly n bytes from src to dst through buf. A short source is
// ErrCorrupt; a failing destination returns its own error. Nothing is ever
// allocated from a length field found in a file.
func copyN(dst io.Writer, src io.Reader, n int64, buf []byte) error {
	for n > 0 {
		k := int(min(n, int64(len(buf))))
		if _, err := io.ReadFull(src, buf[:k]); err != nil {
			return ErrCorrupt
		}
		if _, err := dst.Write(buf[:k]); err != nil {
			return err
		}
		n -= int64(k)
	}
	return nil
}

// readHead reads up to maxEXIF of the next n bytes, discards the rest, and
// returns what it read. Every byte read is also written to also.
func readHead(br *bufio.Reader, n int64, also io.Writer, buf []byte) ([]byte, error) {
	head := make([]byte, min(n, maxEXIF))
	if _, err := io.ReadFull(br, head); err != nil {
		return nil, ErrCorrupt
	}
	if _, err := also.Write(head); err != nil {
		return nil, err
	}
	return head, copyN(also, br, n-int64(len(head)), buf)
}

// stripPNG streams r to w keeping only IHDR, PLTE, tRNS, gAMA, sRGB, IDAT,
// IEND. Chunk lengths and CRCs are validated on input. It returns the
// orientation from an eXIf chunk (0 if none).
func stripPNG(w io.Writer, r io.Reader) (orientation int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	bw := bufio.NewWriterSize(w, 64<<10)
	sig := make([]byte, 8)
	if _, err := io.ReadFull(br, sig); err != nil || string(sig) != string(pngSig) {
		return 0, ErrCorrupt
	}
	if _, err := bw.Write(sig); err != nil {
		return 0, err
	}
	var hdr [8]byte
	var crcb [4]byte
	buf := make([]byte, copyBuf)
	for first := true; ; first = false {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return 0, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		typ := string(hdr[4:8])
		if n > maxPNGChunk || (first && typ != "IHDR") {
			return 0, ErrCorrupt
		}
		crc := crc32.NewIEEE()
		_, _ = crc.Write(hdr[4:8])
		keep := pngKeep(typ, n)
		var exif []byte
		switch {
		case keep:
			if _, err := bw.Write(hdr[:]); err != nil {
				return 0, err
			}
			err = copyN(io.MultiWriter(crc, bw), br, int64(n), buf)
		case typ == "eXIf" && orientation == 0:
			exif, err = readHead(br, int64(n), crc, buf)
		default:
			err = copyN(crc, br, int64(n), buf)
		}
		if err != nil {
			return 0, err
		}
		if _, err := io.ReadFull(br, crcb[:]); err != nil || crc.Sum32() != binary.BigEndian.Uint32(crcb[:]) {
			return 0, ErrCorrupt
		}
		if keep {
			if _, err := bw.Write(crcb[:]); err != nil {
				return 0, err
			}
		}
		if exif != nil {
			orientation = exifOrientation(exif)
		}
		if typ == "IEND" {
			return orientation, bw.Flush() // trailing bytes dropped
		}
	}
}
