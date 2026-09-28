package wire

// Noise handshake payloads (§3.4). Each is encrypted by Noise; these are the
// plaintext layouts. Every handshake message has exactly one legal length.

// HS1 is the msg1 payload.
type HS1 struct{ KemEK [KEMEncapsulationKeySize]byte }

// HS2 is the msg2 payload.
type HS2 struct {
	KemCT [KEMCiphertextSize]byte
	R     [HSRandomSize]byte
}

// HS3 is the msg3 payload; Token is all zeros when HasToken is false.
type HS3 struct {
	R        [HSRandomSize]byte
	HasToken bool
	Token    [InviteTokenSz]byte
}

// Payload sizes.
const (
	HS1PayloadLen = KEMEncapsulationKeySize
	HS2PayloadLen = KEMCiphertextSize + HSRandomSize
	HS3PayloadLen = HSRandomSize + 1 + InviteTokenSz
)

// AppendHS1 appends the msg1 payload.
func AppendHS1(dst []byte, h *HS1) []byte { return append(dst, h.KemEK[:]...) }

// DecodeHS1 parses the msg1 payload.
func DecodeHS1(p []byte, h *HS1) error {
	if len(p) != HS1PayloadLen {
		return lenErr(p, HS1PayloadLen)
	}
	copy(h.KemEK[:], p)
	return nil
}

// AppendHS2 appends the msg2 payload.
func AppendHS2(dst []byte, h *HS2) []byte { return append(append(dst, h.KemCT[:]...), h.R[:]...) }

// DecodeHS2 parses the msg2 payload.
func DecodeHS2(p []byte, h *HS2) error {
	if len(p) != HS2PayloadLen {
		return lenErr(p, HS2PayloadLen)
	}
	copy(h.KemCT[:], p)
	copy(h.R[:], p[KEMCiphertextSize:])
	return nil
}

// AppendHS3 appends the msg3 payload. Token is zeroed on the wire when HasToken is false.
func AppendHS3(dst []byte, h *HS3) []byte {
	dst = append(dst, h.R[:]...)
	if h.HasToken {
		return append(append(dst, 1), h.Token[:]...)
	}
	var zero [InviteTokenSz]byte
	return append(append(dst, 0), zero[:]...)
}

// DecodeHS3 parses the msg3 payload. has_token must be 0 or 1; when 0 the token
// bytes must be zero.
func DecodeHS3(p []byte, h *HS3) error {
	if len(p) != HS3PayloadLen {
		return lenErr(p, HS3PayloadLen)
	}
	copy(h.R[:], p)
	switch p[HSRandomSize] {
	case 0:
		h.HasToken = false
		for _, b := range p[HSRandomSize+1:] {
			if b != 0 {
				return ErrValue
			}
		}
		h.Token = [InviteTokenSz]byte{}
	case 1:
		h.HasToken = true
		copy(h.Token[:], p[HSRandomSize+1:])
	default:
		return ErrValue
	}
	return nil
}

// HSMessageLen returns the one legal Noise message length for handshake
// message i (1..3), or 0.
func HSMessageLen(i int) int {
	switch i {
	case 1:
		return HS1Len
	case 2:
		return HS2Len
	case 3:
		return HS3Len
	}
	return 0
}
