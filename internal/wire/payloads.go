package wire

import (
	"encoding/binary"
	"unicode/utf8"
)

// Every Decode* function requires the payload to decode exactly (no trailing
// bytes) and returns ErrTrailing/ErrShort otherwise. Byte-slice fields alias
// the input; callers must copy before the read buffer is reused.

// Hello is the first frame from each side (§4.4).
type Hello struct {
	Features uint64
	MaxImage uint64
	Name     []byte // ≤ MaxHelloName bytes, valid UTF-8; sanitized by core
}

// AppendHello appends a HELLO payload.
func AppendHello(dst []byte, h Hello) ([]byte, error) {
	if len(h.Name) > MaxHelloName {
		return dst, ErrTooLarge
	}
	dst = binary.BigEndian.AppendUint64(dst, h.Features)
	dst = binary.BigEndian.AppendUint64(dst, h.MaxImage)
	dst = append(dst, byte(len(h.Name))) // #nosec G115 -- ≤ MaxHelloName checked above
	return append(dst, h.Name...), nil
}

// DecodeHello parses and validates a HELLO payload.
func DecodeHello(p []byte) (Hello, error) {
	if len(p) < 17 {
		return Hello{}, ErrShort
	}
	h := Hello{Features: binary.BigEndian.Uint64(p), MaxImage: binary.BigEndian.Uint64(p[8:])}
	n := int(p[16])
	if len(p) != 17+n {
		return Hello{}, ErrTrailing
	}
	if n > MaxHelloName {
		return Hello{}, ErrTooLarge
	}
	if h.Features&^FeatureMask != 0 {
		return Hello{}, ErrValue
	}
	if (h.Features&FeatureImages == 0) != (h.MaxImage == 0) {
		return Hello{}, ErrValue
	}
	h.Name = p[17:]
	if !utf8.Valid(h.Name) {
		return Hello{}, ErrUTF8
	}
	return h, nil
}

// AppendPing appends a PING payload (nonce).
func AppendPing(dst []byte, nonce uint64) []byte { return binary.BigEndian.AppendUint64(dst, nonce) }

// AppendPong appends a PONG payload (echoed nonce).
func AppendPong(dst []byte, nonce uint64) []byte { return binary.BigEndian.AppendUint64(dst, nonce) }

// DecodePing parses a PING payload.
func DecodePing(p []byte) (uint64, error) { return u64(p) }

// DecodePong parses a PONG payload.
func DecodePong(p []byte) (uint64, error) { return u64(p) }

func u64(p []byte) (uint64, error) {
	if len(p) != 8 {
		return 0, lenErr(p, 8)
	}
	return binary.BigEndian.Uint64(p), nil
}

func u8max(p []byte, maxVal uint8) (uint8, error) {
	if len(p) != 1 {
		return 0, lenErr(p, 1)
	}
	if p[0] > maxVal {
		return 0, ErrValue
	}
	return p[0], nil
}

func lenErr(p []byte, want int) error {
	if len(p) < want {
		return ErrShort
	}
	return ErrTrailing
}

// AppendBye appends a BYE payload.
func AppendBye(dst []byte, reason uint8) []byte { return append(dst, reason) }

// DecodeBye parses a BYE payload (reason ≤ ByeResourceLimit).
func DecodeBye(p []byte) (uint8, error) { return u8max(p, ByeResourceLimit) }

// Text is a chat message.
type Text struct {
	MsgID  uint64
	SentAt int64 // unix seconds; 0 when timestamps are disabled
	Text   []byte
}

// AppendText appends a TEXT payload.
func AppendText(dst []byte, t Text) ([]byte, error) {
	if len(t.Text) > MaxTextBytes {
		return dst, ErrTooLarge
	}
	dst = binary.BigEndian.AppendUint64(dst, t.MsgID)
	dst = binary.BigEndian.AppendUint64(dst, uint64(t.SentAt)) // #nosec G115 -- two's complement i64 on the wire
	return append(dst, t.Text...), nil
}

// DecodeText parses a TEXT payload; the text must be valid UTF-8 and ≤ MaxTextBytes.
func DecodeText(p []byte) (Text, error) {
	if len(p) < 16 {
		return Text{}, ErrShort
	}
	if len(p)-16 > MaxTextBytes {
		return Text{}, ErrTooLarge
	}
	t := Text{MsgID: binary.BigEndian.Uint64(p), SentAt: int64(binary.BigEndian.Uint64(p[8:])), Text: p[16:]} // #nosec G115 -- i64 on the wire
	if !utf8.Valid(t.Text) {
		return Text{}, ErrUTF8
	}
	return t, nil
}

// Ack acknowledges a TEXT.
type Ack struct {
	MsgID  uint64
	Status uint8
}

// AppendAck appends an ACK payload.
func AppendAck(dst []byte, a Ack) []byte {
	return append(binary.BigEndian.AppendUint64(dst, a.MsgID), a.Status)
}

// DecodeAck parses an ACK payload (status must be AckDelivered).
func DecodeAck(p []byte) (Ack, error) {
	if len(p) != 9 {
		return Ack{}, lenErr(p, 9)
	}
	if p[8] != AckDelivered {
		return Ack{}, ErrValue
	}
	return Ack{MsgID: binary.BigEndian.Uint64(p), Status: p[8]}, nil
}

// AppendTyping appends a TYPING payload.
func AppendTyping(dst []byte, state uint8) []byte { return append(dst, state) }

// DecodeTyping parses a TYPING payload (0 stop, 1 start).
func DecodeTyping(p []byte) (uint8, error) { return u8max(p, TypingStart) }

// ImgOffer offers an image transfer on a new stream.
type ImgOffer struct {
	Size    uint64
	Format  uint8
	Width   uint32
	Height  uint32
	Hash    [HashSize]byte
	Caption []byte // ≤ MaxCaptionBytes, valid UTF-8, sanitized by core
}

// AppendImgOffer appends an IMG_OFFER payload.
func AppendImgOffer(dst []byte, o ImgOffer) ([]byte, error) {
	if len(o.Caption) > MaxCaptionBytes {
		return dst, ErrTooLarge
	}
	dst = binary.BigEndian.AppendUint64(dst, o.Size)
	dst = append(dst, o.Format)
	dst = binary.BigEndian.AppendUint32(dst, o.Width)
	dst = binary.BigEndian.AppendUint32(dst, o.Height)
	dst = append(dst, o.Hash[:]...)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(o.Caption))) // #nosec G115 -- ≤ MaxCaptionBytes checked above
	return append(dst, o.Caption...), nil
}

const imgOfferFixed = 8 + 1 + 4 + 4 + HashSize + 2

// DecodeImgOffer parses an IMG_OFFER payload. It enforces size ≥ 1, a known
// format, and caption bounds/UTF-8; dimension gates are media's job.
func DecodeImgOffer(p []byte) (ImgOffer, error) {
	if len(p) < imgOfferFixed {
		return ImgOffer{}, ErrShort
	}
	o := ImgOffer{Size: binary.BigEndian.Uint64(p), Format: p[8],
		Width: binary.BigEndian.Uint32(p[9:]), Height: binary.BigEndian.Uint32(p[13:])}
	copy(o.Hash[:], p[17:17+HashSize])
	n := int(binary.BigEndian.Uint16(p[17+HashSize:]))
	if len(p) != imgOfferFixed+n {
		return ImgOffer{}, ErrTrailing
	}
	if n > MaxCaptionBytes {
		return ImgOffer{}, ErrTooLarge
	}
	if o.Size == 0 || o.Format < FormatPNG || o.Format > FormatGIF {
		return ImgOffer{}, ErrValue
	}
	o.Caption = p[imgOfferFixed:]
	if !utf8.Valid(o.Caption) {
		return ImgOffer{}, ErrUTF8
	}
	return o, nil
}

// AppendImgAccept appends an IMG_ACCEPT payload.
func AppendImgAccept(dst []byte, startChunk uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, startChunk)
}

// DecodeImgAccept parses an IMG_ACCEPT payload.
func DecodeImgAccept(p []byte) (uint32, error) {
	if len(p) != 4 {
		return 0, lenErr(p, 4)
	}
	return binary.BigEndian.Uint32(p), nil
}

// AppendImgReject appends an IMG_REJECT payload.
func AppendImgReject(dst []byte, reason uint8) []byte { return append(dst, reason) }

// DecodeImgReject parses an IMG_REJECT payload.
func DecodeImgReject(p []byte) (uint8, error) { return u8max(p, RejectBusy) }

// ImgChunk carries one chunk of image data.
type ImgChunk struct {
	Index uint32
	Data  []byte
}

// AppendImgChunk appends an IMG_CHUNK payload.
func AppendImgChunk(dst []byte, c ImgChunk) ([]byte, error) {
	if len(c.Data) == 0 || len(c.Data) > ChunkData {
		return dst, ErrTooLarge
	}
	return append(binary.BigEndian.AppendUint32(dst, c.Index), c.Data...), nil
}

// DecodeImgChunk parses an IMG_CHUNK payload (1 ≤ len(data) ≤ ChunkData). The
// exact expected length for a given index is checked by the stream state machine.
func DecodeImgChunk(p []byte) (ImgChunk, error) {
	if len(p) < 5 {
		return ImgChunk{}, ErrShort
	}
	if len(p)-4 > ChunkData {
		return ImgChunk{}, ErrTooLarge
	}
	return ImgChunk{Index: binary.BigEndian.Uint32(p), Data: p[4:]}, nil
}

// DecodeEmpty parses an empty payload (IMG_DONE, REKEY_DONE, REKEY_REQUEST).
func DecodeEmpty(p []byte) error {
	if len(p) != 0 {
		return ErrTrailing
	}
	return nil
}

// AppendImgResult appends an IMG_RESULT payload.
func AppendImgResult(dst []byte, status uint8) []byte { return append(dst, status) }

// DecodeImgResult parses an IMG_RESULT payload.
func DecodeImgResult(p []byte) (uint8, error) { return u8max(p, ResultAborted) }

// AppendImgCancel appends an IMG_CANCEL payload.
func AppendImgCancel(dst []byte, reason uint8) []byte { return append(dst, reason) }

// DecodeImgCancel parses an IMG_CANCEL payload.
func DecodeImgCancel(p []byte) (uint8, error) { return u8max(p, CancelShutdown) }

// RekeyInit starts a rekey (initiator → responder).
type RekeyInit struct {
	EPub  [X25519Size]byte
	KemEK [KEMEncapsulationKeySize]byte
}

// AppendRekeyInit appends a REKEY_INIT payload.
func AppendRekeyInit(dst []byte, r *RekeyInit) []byte {
	return append(append(dst, r.EPub[:]...), r.KemEK[:]...)
}

// DecodeRekeyInit parses a REKEY_INIT payload into r.
func DecodeRekeyInit(p []byte, r *RekeyInit) error {
	if len(p) != X25519Size+KEMEncapsulationKeySize {
		return lenErr(p, X25519Size+KEMEncapsulationKeySize)
	}
	copy(r.EPub[:], p)
	copy(r.KemEK[:], p[X25519Size:])
	return nil
}

// RekeyResp answers a REKEY_INIT (responder → initiator).
type RekeyResp struct {
	EPub  [X25519Size]byte
	KemCT [KEMCiphertextSize]byte
}

// AppendRekeyResp appends a REKEY_RESP payload.
func AppendRekeyResp(dst []byte, r *RekeyResp) []byte {
	return append(append(dst, r.EPub[:]...), r.KemCT[:]...)
}

// DecodeRekeyResp parses a REKEY_RESP payload into r.
func DecodeRekeyResp(p []byte, r *RekeyResp) error {
	if len(p) != X25519Size+KEMCiphertextSize {
		return lenErr(p, X25519Size+KEMCiphertextSize)
	}
	copy(r.EPub[:], p)
	copy(r.KemCT[:], p[X25519Size:])
	return nil
}
