package wire

import (
	"encoding/binary"
	"errors"
)

// Errors. Every decode failure is a protocol violation that closes the
// session; the distinct values exist for tests and redacted logs only.
var (
	ErrOuterLength   = errors.New("wire: outer length out of range")
	ErrShort         = errors.New("wire: input too short")
	ErrFlags         = errors.New("wire: non-zero flags")
	ErrPayloadLength = errors.New("wire: payload length out of range")
	ErrPadding       = errors.New("wire: frame length does not match padding rule")
	ErrTrailing      = errors.New("wire: payload does not decode exactly")
	ErrType          = errors.New("wire: unknown frame type")
	ErrStream        = errors.New("wire: wrong stream for frame type")
	ErrValue         = errors.New("wire: field value out of range")
	ErrUTF8          = errors.New("wire: invalid UTF-8")
	ErrTooLarge      = errors.New("wire: value exceeds limit")
)

// OuterLen parses the 4-byte length prefix and checks its bounds. The caller
// must not read the body unless err == nil.
func OuterLen(hdr []byte) (int, error) {
	if len(hdr) < OuterLenSize {
		return 0, ErrShort
	}
	n := binary.BigEndian.Uint32(hdr)
	if n < MinCiphertext || n > MaxCiphertext {
		return 0, ErrOuterLength
	}
	return int(n), nil
}

// PutOuterLen writes the length prefix. n must be within bounds (the writer
// only ever produces valid lengths; a violation is a bug, reported as error).
func PutOuterLen(dst []byte, n int) error {
	if len(dst) < OuterLenSize {
		return ErrShort
	}
	if n < MinCiphertext || n > MaxCiphertext {
		return ErrOuterLength
	}
	binary.BigEndian.PutUint32(dst, uint32(n))
	return nil
}

// PaddedLen returns the padded inner-frame length for a payload of payloadLen
// bytes (§4.2): the smallest multiple of 256 ≥ 8+payloadLen when that is ≤ 4096,
// otherwise the smallest multiple of 4096. Returns 0 if payloadLen > MaxPayload.
func PaddedLen(payloadLen int) int {
	if payloadLen < 0 || payloadLen > MaxPayload {
		return 0
	}
	inner := InnerHeaderSize + payloadLen
	bucket := SmallBucket
	if inner > SmallBucketMax {
		bucket = LargeBucket
	}
	return (inner + bucket - 1) / bucket * bucket
}

// Inner is a decoded inner frame. Payload aliases the input buffer.
type Inner struct {
	Type    FrameType
	Stream  uint16
	Payload []byte
}

// StreamClass says which streams a frame type may travel on.
type StreamClass uint8

// Stream classes.
const (
	ClassUnknown  StreamClass = iota
	ClassControl              // stream 0
	ClassChat                 // stream 1
	ClassTransfer             // stream ≥ 2
)

// Class returns the stream class for t, or ClassUnknown for an unknown type.
func (t FrameType) Class() StreamClass {
	switch t {
	case TypeHello, TypePing, TypePong, TypeBye,
		TypeRekeyInit, TypeRekeyResp, TypeRekeyDone, TypeRekeyRequest:
		return ClassControl
	case TypeText, TypeAck, TypeTyping:
		return ClassChat
	case TypeImgOffer, TypeImgAccept, TypeImgReject, TypeImgChunk,
		TypeImgDone, TypeImgResult, TypeImgCancel:
		return ClassTransfer
	}
	return ClassUnknown
}

func streamOK(t FrameType, s uint16) bool {
	switch t.Class() {
	case ClassControl:
		return s == StreamControl
	case ClassChat:
		return s == StreamChat
	case ClassTransfer:
		return s >= StreamMinTransfer
	}
	return false
}

// EncodeInner appends the inner frame (header, payload, zero padding) to dst
// and returns the extended slice. It fails only on a caller bug (payload too
// large or a type/stream mismatch).
func EncodeInner(dst []byte, typ FrameType, stream uint16, payload []byte) ([]byte, error) {
	if !streamOK(typ, stream) {
		return dst, ErrStream
	}
	total := PaddedLen(len(payload))
	if total == 0 {
		return dst, ErrPayloadLength
	}
	start := len(dst)
	dst = grow(dst, total)
	b := dst[start:]
	b[0] = byte(typ)
	b[1] = 0
	binary.BigEndian.PutUint16(b[2:], stream)
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload))) // #nosec G115 -- ≤ MaxPayload via PaddedLen
	copy(b[InnerHeaderSize:], payload)
	clear(b[InnerHeaderSize+len(payload):])
	return dst, nil
}

// DecodeInner parses a decrypted inner frame and enforces flags == 0, the
// payload bound, the padding rule and the type/stream pairing. Payload aliases b.
func DecodeInner(b []byte) (Inner, error) {
	if len(b) < InnerHeaderSize {
		return Inner{}, ErrShort
	}
	if b[1] != 0 {
		return Inner{}, ErrFlags
	}
	typ := FrameType(b[0])
	stream := binary.BigEndian.Uint16(b[2:])
	plen := binary.BigEndian.Uint32(b[4:])
	if plen > MaxPayload {
		return Inner{}, ErrPayloadLength
	}
	if len(b) != PaddedLen(int(plen)) {
		return Inner{}, ErrPadding
	}
	if typ.Class() == ClassUnknown {
		return Inner{}, ErrType
	}
	if !streamOK(typ, stream) {
		return Inner{}, ErrStream
	}
	return Inner{Type: typ, Stream: stream, Payload: b[InnerHeaderSize : InnerHeaderSize+int(plen)]}, nil
}

// FrameAD writes the AEAD associated data for a frame into dst (len ≥ ADSize)
// and returns it: prefix || epoch(u32) || counter(u64) || direction(u8).
func FrameAD(dst []byte, epoch uint32, counter uint64, direction uint8) []byte {
	dst = dst[:0]
	dst = append(dst, FrameADPrefix...)
	dst = binary.BigEndian.AppendUint32(dst, epoch)
	dst = binary.BigEndian.AppendUint64(dst, counter)
	return append(dst, direction)
}

// ADSize is len(FrameAD(...)).
const ADSize = len(FrameADPrefix) + 4 + 8 + 1

// FrameNonce writes the 12-byte AEAD nonce 0x00000000 || counter(u64 BE).
func FrameNonce(dst *[NonceSize]byte, counter uint64) {
	dst[0], dst[1], dst[2], dst[3] = 0, 0, 0, 0
	binary.BigEndian.PutUint64(dst[4:], counter)
}

// Direction bytes for FrameAD.
const (
	DirInitiatorToResponder uint8 = 0
	DirResponderToInitiator uint8 = 1
)

func grow(b []byte, n int) []byte {
	if cap(b)-len(b) >= n {
		return b[:len(b)+n]
	}
	nb := make([]byte, len(b)+n, 2*cap(b)+n)
	copy(nb, b)
	return nb
}
