package media

import (
	"bufio"
	"encoding/binary"
	"hash/crc32"
	"io"
)

var pngSig = []byte("\x89PNG\r\n\x1a\n")

func pngKeep(t string) bool {
	switch t {
	case "IHDR", "PLTE", "tRNS", "gAMA", "sRGB", "IDAT", "IEND":
		return true
	}
	return false
}

const maxPNGChunk = 1 << 31

// stripPNG streams r to w keeping only IHDR, PLTE, tRNS, gAMA, sRGB, IDAT,
// IEND. Chunk lengths and CRCs are validated on input and recomputed on output.
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
	first := true
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return 0, ErrCorrupt
		}
		n := binary.BigEndian.Uint32(hdr[:4])
		if n > maxPNGChunk {
			return 0, ErrCorrupt
		}
		typ := string(hdr[4:8])
		if first && typ != "IHDR" {
			return 0, ErrCorrupt
		}
		first = false
		data := make([]byte, n)
		if _, err := io.ReadFull(br, data); err != nil {
			return 0, ErrCorrupt
		}
		var crcb [4]byte
		if _, err := io.ReadFull(br, crcb[:]); err != nil {
			return 0, ErrCorrupt
		}
		crc := crc32.NewIEEE()
		_, _ = crc.Write(hdr[4:8])
		_, _ = crc.Write(data)
		if crc.Sum32() != binary.BigEndian.Uint32(crcb[:]) {
			return 0, ErrCorrupt
		}
		if typ == "eXIf" && orientation == 0 {
			orientation = exifOrientation(data)
		}
		if pngKeep(typ) {
			binary.BigEndian.PutUint32(crcb[:], crc.Sum32())
			if _, err := bw.Write(hdr[:]); err != nil {
				return 0, err
			}
			if _, err := bw.Write(data); err != nil {
				return 0, err
			}
			if _, err := bw.Write(crcb[:]); err != nil {
				return 0, err
			}
		}
		if typ == "IEND" {
			return orientation, bw.Flush() // trailing bytes dropped
		}
	}
}
