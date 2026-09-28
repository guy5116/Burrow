package media

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
)

const (
	vp8xICC  = 0x20
	vp8xEXIF = 0x08
	vp8xXMP  = 0x04
	vp8xAnim = 0x02
)

// webpAnimated reports whether the VP8X flags declare animation (or an ANIM chunk exists).
func webpAnimated(data []byte) bool {
	if len(data) < 30 || string(data[12:16]) != "VP8X" {
		return bytes.Contains(data, []byte("ANIM"))
	}
	return data[20]&vp8xAnim != 0
}

// stripWebP drops EXIF, XMP and ICCP chunks, clears the matching VP8X flag
// bits and rewrites the RIFF size. Animated files are rejected.
func stripWebP(w io.Writer, r io.Reader) (orientation int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	hdr := make([]byte, 12)
	if _, err := io.ReadFull(br, hdr); err != nil || string(hdr[:4]) != "RIFF" || string(hdr[8:12]) != "WEBP" {
		return 0, ErrCorrupt
	}
	riffSize := int(binary.LittleEndian.Uint32(hdr[4:]))
	if riffSize < 4 {
		return 0, ErrCorrupt
	}
	remaining := riffSize - 4
	var out bytes.Buffer
	out.Write([]byte("WEBP"))
	vp8xAt := -1
	for remaining > 0 {
		var ch [8]byte
		if _, err := io.ReadFull(br, ch[:]); err != nil {
			return 0, ErrCorrupt
		}
		fourcc := string(ch[:4])
		n := int(binary.LittleEndian.Uint32(ch[4:]))
		padded := n + n&1
		if padded+8 > remaining {
			return 0, ErrCorrupt
		}
		remaining -= 8 + padded
		data := make([]byte, padded)
		if _, err := io.ReadFull(br, data); err != nil {
			return 0, ErrCorrupt
		}
		switch fourcc {
		case "ANIM", "ANMF":
			return 0, ErrAnimated
		case "EXIF":
			if orientation == 0 {
				t := data[:n]
				if len(t) >= 6 && string(t[:6]) == "Exif\x00\x00" {
					t = t[6:]
				}
				orientation = exifOrientation(t)
			}
		case "XMP ", "ICCP":
		case "VP8X":
			if n < 10 {
				return 0, ErrCorrupt
			}
			if data[0]&vp8xAnim != 0 {
				return 0, ErrAnimated
			}
			data[0] &^= vp8xICC | vp8xEXIF | vp8xXMP
			vp8xAt = out.Len()
			out.Write(ch[:])
			out.Write(data)
		default: // VP8, VP8L, ALPH and unknown non-metadata chunks
			out.Write(ch[:])
			out.Write(data)
		}
	}
	_ = vp8xAt
	var riff [8]byte
	copy(riff[:4], "RIFF")
	binary.LittleEndian.PutUint32(riff[4:], uint32(out.Len())) // #nosec G115 -- bounded by MaxImage
	if _, err := w.Write(riff[:]); err != nil {
		return 0, err
	}
	_, err = w.Write(out.Bytes())
	return orientation, err
}
