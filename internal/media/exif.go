package media

import "encoding/binary"

// exifOrientation parses a TIFF blob (the bytes after "Exif\0\0") and returns
// the IFD0 Orientation tag (1–8), or 0 when absent or malformed. It never
// panics on hostile input.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:]) != 42 {
		return 0
	}
	off := int(bo.Uint32(tiff[4:]))
	if off < 8 || off > len(tiff)-2 {
		return 0
	}
	n := int(bo.Uint16(tiff[off:]))
	if n > 512 {
		return 0
	}
	off += 2
	for i := 0; i < n; i++ {
		e := off + i*12
		if e+12 > len(tiff) {
			return 0
		}
		tag, typ, count := bo.Uint16(tiff[e:]), bo.Uint16(tiff[e+2:]), bo.Uint32(tiff[e+4:])
		if tag != 0x0112 {
			continue
		}
		if typ != 3 || count != 1 { // SHORT ×1
			return 0
		}
		v := int(bo.Uint16(tiff[e+8:]))
		if v < 1 || v > 8 {
			return 0
		}
		return v
	}
	return 0
}
