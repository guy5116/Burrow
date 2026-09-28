// Package wire is the only place that knows frame byte layouts. It exports
// every protocol constant by name and provides byte-exact, allocation-free,
// bounds-checked encode/decode for every frame. Parsers are total: any input
// yields (value, error), never a panic. It imports only the standard library.
package wire

import "time"

// Protocol identity.
const (
	ProtocolVersion = 1
	Prologue        = "burrow/1"
	FrameADPrefix   = "burrow-frame-v1"
	ChainsInfo      = "burrow/1 chains"
)

// Noise handshake message sizes (the Noise message itself, without the u16 length prefix).
const (
	HS1Len        = 1232 // e(32) || AEAD(kem_ek[1184])
	HS2Len        = 1168 // e(32) || AEAD(kem_ct[1088] || r_r[32])
	HS3Len        = 113  // AEAD(s)(48) || AEAD(r_i[32] || has_token u8 || token[16])
	HSLenPrefix   = 2
	HSRandomSize  = 32 // r_i, r_r
	InviteTokenSz = 16
)

// ML-KEM-768 sizes.
const (
	KEMEncapsulationKeySize = 1184
	KEMCiphertextSize       = 1088
	KEMSeedSize             = 64
	KEMSharedSecretSize     = 32
)

// Key sizes.
const (
	X25519Size   = 32
	ChainKeySize = 32
	RootKeySize  = 32
	HashSize     = 32 // BLAKE2b-256
)

// Frame sizes (§4.1, §4.2).
const (
	OuterLenSize    = 4
	TagSize         = 16
	InnerHeaderSize = 8
	MaxPayload      = 65528
	MaxInner        = 65536
	MinInner        = 256
	SmallBucket     = 256
	LargeBucket     = 4096
	SmallBucketMax  = 4096
	MinCiphertext   = MinInner + TagSize // 272
	MaxCiphertext   = MaxInner + TagSize // 65552
	MaxOuterFrame   = OuterLenSize + MaxCiphertext
	EpochMaxFrames  = 65536 // counter cap: refuse to encrypt/decrypt at this counter
	NonceSize       = 12
)

// Streams (§4.3).
const (
	StreamControl     uint16 = 0
	StreamChat        uint16 = 1
	StreamMinTransfer uint16 = 2
)

// FrameType is the inner frame type byte.
type FrameType uint8

// Frame types (§4.4).
const (
	TypeHello        FrameType = 0x01
	TypePing         FrameType = 0x02
	TypePong         FrameType = 0x03
	TypeBye          FrameType = 0x04
	TypeText         FrameType = 0x10
	TypeAck          FrameType = 0x11
	TypeTyping       FrameType = 0x12
	TypeImgOffer     FrameType = 0x20
	TypeImgAccept    FrameType = 0x21
	TypeImgReject    FrameType = 0x22
	TypeImgChunk     FrameType = 0x23
	TypeImgDone      FrameType = 0x24
	TypeImgResult    FrameType = 0x25
	TypeImgCancel    FrameType = 0x26
	TypeRekeyInit    FrameType = 0x30
	TypeRekeyResp    FrameType = 0x31
	TypeRekeyDone    FrameType = 0x32
	TypeRekeyRequest FrameType = 0x33
)

// HELLO.
const (
	FeatureImages      uint64 = 1 << 0
	FeatureTyping      uint64 = 1 << 1
	FeatureAnimatedGIF uint64 = 1 << 2
	FeatureMask        uint64 = FeatureImages | FeatureTyping | FeatureAnimatedGIF
	MaxHelloName              = 32
)

// TEXT / ACK / TYPING.
const (
	MaxTextBytes  = 16384
	AckDelivered  = 1
	TypingStop    = 0
	TypingStart   = 1
	MsgIDDedupSet = 4096
)

// BYE reasons.
const (
	ByeUserQuit      = 0
	ByeShutdown      = 1
	ByeLocalError    = 2
	ByeReplaced      = 3
	ByeResourceLimit = 4
)

// Image transfer (§4.3, §4.4, §6).
const (
	MaxCaptionBytes     = 1024
	ChunkData           = 65524 // 8 header + 4 index + 65524 = 65536
	FormatPNG           = 1
	FormatJPEG          = 2
	FormatWebP          = 3
	FormatGIF           = 4
	RejectDeclined      = 0
	RejectTooLarge      = 1
	RejectUnsupported   = 2
	RejectBusy          = 3
	ResultOK            = 0
	ResultHashMismatch  = 1
	ResultAborted       = 2
	CancelUser          = 0
	CancelHashDiverged  = 1
	CancelLocalIO       = 2
	CancelShutdown      = 3
	MaxPendingOffers    = 4
	MaxActiveTransfers  = 2
	MaxBusyRejects      = 32 // per 10 minutes → close
	BusyRejectWindow    = 10 * time.Minute
	MaxOffersPerWindow  = 16 // per 60 s → close
	OfferWindow         = time.Minute
	MaxTypingPerWindow  = 32 // per 60 s → drop
	TypingWindow        = time.Minute
	MaxDrainingStreams  = 16
	ClosedStreamMemory  = 64
	DefaultMaxImage     = 25 << 20
	MaxImagePixels      = 40_000_000
	MaxImageSide        = 16384
	MaxImageDecodeBytes = 256 << 20
	MaxGIFFrames        = 200
	MaxConcurrentDecode = 2
	FreeSpaceMargin     = 64 << 20
)

// Rekey schedule and fail-safes (§3.5).
const (
	RekeyInitCounter     = 1024
	RekeyInitInterval    = 15 * time.Minute
	RekeyRequestCounter  = 2048
	RekeyRequestInterval = 20 * time.Minute
	RekeyRequestTimeout  = 10 * time.Second
	RekeyEpochFailSafe   = 25 * time.Minute
	RekeyInFlightTimeout = 30 * time.Second
	ControllerTick       = time.Second
)

// Keepalive and deadlines (§4.5, §12).
const (
	PingInterval        = 30 * time.Second
	DeadPeerTimeout     = 90 * time.Second
	WriteDeadline       = 30 * time.Second
	HandshakeTimeout    = 10 * time.Second
	HandshakeMsg1Wait   = 5 * time.Second
	ByeGracePeriod      = 2 * time.Second
	AcceptPromptTimeout = 10 * time.Minute
	ReconnectMinBackoff = time.Second
	ReconnectMaxBackoff = 60 * time.Second
	GlareWindow         = 5 * time.Second
)

// Handshake and connection limits (§3.4, §12).
const (
	MaxPendingHandshakes = 64
	Msg1ReplayLRU        = 4096
	TCPFailuresPerMinute = 10
	TorFailuresPerMinute = 60
	RateLimitPenalty     = 10 * time.Minute
	TorLimitedSlots      = 16
	MaxPeers             = 32
	MaxPingsPerWindow    = 4 // per 10 s → close
	PingFloodWindow      = 10 * time.Second
	ControlQueueCap      = 8
	ChatQueueCap         = 64
	TransferQueueCap     = 2
	ControlQueueStall    = 5 * time.Second
	MaxQueuedMessages    = 1000
)

// Contacts and invites (§3.3, §4.6).
const (
	MaxContacts          = 1024
	MaxContactsPerInvite = 64
	DefaultInviteTTL     = time.Hour
	DefaultListenPort    = 47337
)
