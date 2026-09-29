# Burrow protocol, version 1

This document is normative for bytes on the wire. `internal/wire` is the reference
codec; `make docs-check` verifies that every exported constant there matches the table
in §9. All integers are big-endian. CLAUDE.md §3–§4 is the design rationale; this file
is the byte-level contract an independent implementation needs.

## 1. Primitives

| Purpose | Primitive |
|---|---|
| Handshake | Noise `XK_25519_ChaChaPoly_BLAKE2s`, prologue `"burrow/1"` |
| Identity | X25519 static key (raw 32-byte scalar); `PeerID` = the 32-byte public key |
| Post-quantum KEM | ML-KEM-768 (FIPS 203) |
| KDF | HKDF-SHA-256 (RFC 5869); HMAC-SHA-256 chains |
| Frame AEAD | ChaCha20-Poly1305 (IETF, 12-byte nonce) |
| Hash | BLAKE2b-256 (files, invites, mDNS), BLAKE2b-512 (safety numbers) |

There is exactly one suite per protocol major version. No negotiation.

## 2. Identity and verification

- Fingerprint: the 32-byte public key as lowercase, unpadded RFC 4648 base32 (52
  characters), displayed in groups of 4. Parsing is canonical: trailing bits must be zero.
- Safety number: `BLAKE2b-512("burrow/1 safety" || min(pkA, pkB) || max(pkA, pkB))`, first
  60 bytes as twelve 5-byte big-endian integers, each `mod 100000`, rendered as twelve
  5-digit groups separated by single spaces, e.g. `84418 61305 05322 …`.
- mDNS tag (opt-in discovery): `BLAKE2b-256("burrow/1 mdns" || nonce[16] || pubkey)[:16]`.

## 3. Handshake

Noise XK. The initiator knows the responder's static key in advance. Each handshake
message is framed as `u16 BE length || message`; the message has exactly one legal
length. Any other length → close without writing.

| Msg | Direction | Noise tokens | Payload (plaintext, then Noise-encrypted) | Message length |
|---|---|---|---|---|
| 1 | I → R | `e, es` | `HS1 { kem_ek [1184] }` | 1232 |
| 2 | R → I | `e, ee` | `HS2 { kem_ct [1088], r_r [32] }` | 1168 |
| 3 | I → R | `s, se` | `HS3 { r_i [32], has_token u8, token [16] }` | 113 |

`has_token` is 0 or 1; when 0, `token` must be all zeros. After msg3 the responder calls
its authorization policy (known contact → ok; unknown with a valid invite token → ok and
the token is reserved; otherwise close silently). The responder also rejects msg1 whose
ephemeral public key was seen in the last 4096 handshakes, before any DH.

Root key derivation (both sides), `h = ChannelBinding()`:

```
ikm    = r_i || r_r || kem_ss                    (96 bytes)
root_0 = HKDF-Extract(sha256, ikm, salt = h)
ck_i2r_0 || ck_r2i_0 = HKDF-Expand(sha256, root_0, "burrow/1 chains" || uint32(0), 64)
```

The Noise CipherStates are discarded. The HELLO exchange (§6) is the key confirmation.

## 4. Chains and frame encryption

Per direction: `mk = HMAC-SHA256(ck, 0x01)`, `ck' = HMAC-SHA256(ck, 0x02)`. Every frame
advances its chain. AEAD: key `mk`, nonce `0x00000000 || counter(u64 BE)`, associated data
`"burrow-frame-v1" || epoch(u32 BE) || counter(u64 BE) || direction(u8)` with direction 0
for initiator→responder and 1 for responder→initiator. Counters start at 0 in every epoch
and are never transmitted. At counter 65,536 both sides refuse and close.

## 5. Frames

Outer: `u32 len || ciphertext`, `272 ≤ len ≤ 65,552`, rejected before the body is read.

Inner (decrypted):

```
u8  type
u8  flags        must be 0
u16 stream
u32 payload_len  ≤ 65,528
[]  payload
[]  padding      zeros; length enforced by the receiver
```

Padding: with `inner = 8 + payload_len`, the total is the smallest multiple of 256 ≥ inner
when inner ≤ 4096, otherwise the smallest multiple of 4096 ≥ inner. Maximum 65,536.

Streams: 0 control, 1 chat, ≥ 2 one per transfer of an image or another file (initiator allocates even ids,
responder odd; never reused). A type on the wrong stream class → close.

| Type | Name | Stream | Payload |
|---|---|---|---|
| 0x01 | HELLO | 0 | `features u64` (bit0 images, bit1 TYPING, bit2 animated GIF, bit3 files), `max_image u64`, `max_file u64`, `name_len u8`, `name` (≤ 32 bytes UTF-8) |
| 0x02 | PING | 0 | `nonce u64` |
| 0x03 | PONG | 0 | `nonce u64` |
| 0x04 | BYE | 0 | `reason u8` (0 quit, 1 shutdown, 2 local error, 3 replaced, 4 resource limit) |
| 0x10 | TEXT | 1 | `msg_id u64`, `sent_at i64`, `text` (UTF-8, ≤ 16,384 bytes) |
| 0x11 | ACK | 1 | `msg_id u64`, `status u8` (1 delivered) |
| 0x12 | TYPING | 1 | `state u8` (0 stop, 1 start) |
| 0x20 | IMG_OFFER | ≥2 | `size u64` (≥1), `format u8` (1 PNG, 2 JPEG, 3 WebP, 4 GIF), `width u32`, `height u32`, `blake2b256 [32]`, `caption_len u16`, `caption` (≤ 1,024 bytes) |
| 0x21 | IMG_ACCEPT | ≥2 | `start_chunk u32` |
| 0x22 | IMG_REJECT | ≥2 | `reason u8` (0 declined, 1 too large, 2 unsupported, 3 busy) |
| 0x23 | IMG_CHUNK | ≥2 | `index u32`, `data` (65,524 bytes except the last chunk) |
| 0x24 | IMG_DONE | ≥2 | empty |
| 0x25 | IMG_RESULT | ≥2 | `status u8` (0 ok, 1 hash mismatch, 2 aborted) |
| 0x26 | IMG_CANCEL | ≥2 | `reason u8` (0 user, 1 hash divergence, 2 local I/O, 3 shutdown) |
| 0x27 | FILE_OFFER | ≥2 | `size u64` (1 … 2^40), `blake2b256 [32]`, `ext_len u8`, `ext` (≤ 8 bytes, only `a`–`z` and `0`–`9`), `caption_len u16`, `caption` (≤ 1,024 bytes) |
| 0x30 | REKEY_INIT | 0 | `e_pub [32]`, `kem_ek [1184]` |
| 0x31 | REKEY_RESP | 0 | `e_pub [32]`, `kem_ct [1088]` |
| 0x32 | REKEY_DONE | 0 | empty |
| 0x33 | REKEY_REQUEST | 0 | empty |

Every payload must decode exactly; trailing bytes, out-of-range enumerations, reserved
HELLO feature bits (only bits 0–3 exist), `max_image` inconsistent with bit 0, `max_file`
inconsistent with bit 3, an offer size above 2^40, an extension byte outside `a`–`z` and
`0`–`9`, and invalid UTF-8 all close the session. Unknown types close the session.

The byte layout above is protocol v1 as amended on 2026-09-29, before any release:
`max_file` and FILE_OFFER were added for file transfer.

## 6. Session state machine

Connection: `Handshaking → Hello → Established → Closing → Closed`. HELLO is each side's
first frame; the initiator sends it right after msg3, the responder answers with its own
and may then send data; the initiator may send data once the responder's HELLO validates.

Rekey (initiator drives; responder may REQUEST):

```
I: REKEY_INIT  (epoch n)            R: derive n+1; REKEY_RESP (epoch n); R's SEND := n+1
I: RECV := n+1 after RESP; REKEY_DONE (epoch n, last epoch-n frame from I); I's SEND := n+1
R: RECV := n+1 after DONE
root_{n+1} = HKDF-Extract(sha256, dh_ee || dh_es || dh_se || kem_ss', salt = root_n)
ck_i2r || ck_r2i = HKDF-Expand(sha256, root_{n+1}, "burrow/1 chains" || uint32(n+1), 64)
```

with `dh_es = X25519(e_i', s_r)` and `dh_se = X25519(s_i, e_r')` computed by the respective
holders (the responder computes `X25519(s_r, e_i'.pub)` and `X25519(e_r', s_i.pub)`). Any
X25519 error (all-zero output) closes the session. A frame's epoch is the sender's SEND
chain at the instant the frame is written; non-rekey frames may be sent at any point
during a rekey.

### 6.1 Control-frame legality (anything else closes the session, without BYE)

| Frame | Sender | Legal when |
|---|---|---|
| HELLO | either | exactly once, as that side's first frame |
| PING | either | after the peer's HELLO; more than 4 PINGs in 10 s → close |
| PONG | either | `nonce` equals the nonce of the one outstanding PING and differs from the last accepted PONG |
| REKEY_INIT | initiator | the responder is not already awaiting REKEY_DONE |
| REKEY_RESP | responder | the initiator has a rekey in flight whose ephemerals are unused |
| REKEY_DONE | initiator | the responder is awaiting it |
| REKEY_REQUEST | responder | any time after both HELLOs; ignored by the initiator while a rekey is in flight |
| BYE | either | any time after both HELLOs |

A receiver treats "after both HELLOs" as "after the peer's HELLO validated".

### 6.2 Schedule and fail-safes

- Initiator starts a rekey when either chain counter reaches 1024 or 15 minutes after its
  SEND chain last switched.
- Responder sends REKEY_REQUEST at 2048 frames or 20 minutes; if no REKEY_INIT follows
  within 10 s it closes.
- Either side closes when 25 minutes pass since a chain last switched, when a rekey has
  been in flight for more than 30 s, or when the responder has awaited REKEY_DONE for
  more than 30 s.
- PING after 30 s of send-side silence, never while one is outstanding. No frame of any
  kind for 90 s → the connection is dead. Write deadline 30 s per frame. Handshake 10 s
  in total, msg1 within 5 s of accept.

### 6.3 Streams

Per-stream states: `Offered → Accepted → Transferring → Done | Draining → Closed`.

- IMG_OFFER or FILE_OFFER opens a stream; the receiver closes the session when its own
  HELLO did not advertise that kind (bit 0, bit 3). A FILE_OFFER transfer then uses the
  same IMG_ACCEPT, IMG_REJECT, IMG_CHUNK, IMG_DONE, IMG_RESULT and IMG_CANCEL frames and
  rules. A file offer carries no name: the receiver stores `file-<hash8>.<ext>`, or `.bin`
  when `ext` is empty, and never opens or decodes it. An offer larger than the receiver's
  advertised limit is answered with IMG_REJECT reason 1. The id must have the offering side's parity (initiator even
  from 2, responder odd from 3), be greater than every id that side used before, and not
  be among the last 64 closed ids.
- IMG_ACCEPT (receiver → sender) is legal once on an offered stream; `start_chunk` must be
  below `chunks = ceil(size / 65524)`.
- IMG_CHUNK `i` must carry index `i` exactly, in order, and 65,524 bytes of data except the
  last, which carries `size − (chunks−1)·65524`. IMG_DONE is legal only after the last chunk.
- IMG_RESULT (receiver → sender) is legal only after IMG_DONE. IMG_REJECT is legal only
  before IMG_ACCEPT. Both close the stream on send and on receipt.
- IMG_CANCEL: the receiver of a CANCEL on an open stream replies with exactly one
  IMG_CANCEL and closes. The first sender of a CANCEL enters draining and discards every
  frame on the stream until a terminal frame arrives. A CANCEL on one of the last 64
  closed ids is ignored; any other frame on a closed or unknown id closes the session.
- Limits per peer and direction: 4 pending offers, 2 active transfers (a further offer is
  answered with IMG_REJECT reason 3 and no user-visible event), 32 busy rejects in 10
  minutes → close, 16 offers in 60 s → close, 16 draining streams, 32 TYPING frames in
  60 s (further ones are dropped).
- Writer priority: control > chat > transfer; concurrent transfers are round-robined and
  every control or chat frame is flushed immediately.

### 6.4 Text handling

Peer strings must be valid UTF-8 (else close). Before display they are sanitized:
C0/C1 controls except `\n` and `\t`, U+202A–202E, U+2066–2069, U+2028, U+2029, U+200B,
U+200E, U+200F, U+2060–2064, U+FEFF, U+061C, U+180E and U+E0000–E007F become U+FFFD;
runs of U+200C/U+200D are capped at 2 (removed from display names); runs of combining
marks (Mn/Me) are capped at 4; in single-line fields `\n` and `\t` become a space.
Name-collision checks compare NFKC → case-fold → remove Cf, Other_Default_Ignorable and
variation selectors → collapse whitespace → trim.

### 6.5 Session replacement

A newly authenticated session for a peer that already has one replaces it (the old one
is closed with BYE reason 3). If the old session is younger than 5 s, both sides keep
the session whose initiator has the lexicographically smaller public key.

### 6.6 Delivery

TEXT `msg_id`s are random 64-bit values. The receiver remembers the last 4096 ids per
peer, drops duplicates, and ACKs every copy. A sender re-sends every unacknowledged TEXT
with its original id, in order, after the next HELLO exchange.

## 7. Invites

```
"burrow1:" || base32-lower-nopad( version u8 = 1 || kind u8 (1 tcp, 2 tor) || addr_len u8 || addr
                                 || port u16 || pubkey [32] || token [16] || expiry i64 || flags u8 (bit0 multi-use)
                                 || check [4] = BLAKE2b-256("burrow/1 invite" || preceding bytes)[:4] )
```

The parser is strict: lowercase only, canonical base32, exact length, `addr_len ≤ 253`,
`port ≥ 1`, non-zero key, `expiry > 0`, no reserved flags, hostname per RFC 1123 (or an
IP literal; for kind 2 a 56-character v3 `.onion`), and the checksum must match.

## 8. At-rest formats

- `master.hdr`: `"BRWM" || version u8 = 1 || mode u8 (1 passphrase, 2 none) || t u32 || m u32 (KiB) || p u8 || salt [16]`
  (31 bytes). Accepted bounds: `1 ≤ t ≤ 16`, `8 MiB ≤ m ≤ 1 GiB`, `1 ≤ p ≤ 16`.
- Blob: `"BRWS" || version u8 = 1 || nonce [24] || XChaCha20-Poly1305(file_key, nonce, ad, plaintext)`,
  `ad = "BRWS" || version || nonce || relpath`, `file_key = HKDF-Expand(sha256, master, "burrow/1 store " || relpath, 32)`.
- Identity blob plaintext: `version u8 = 2 || scalar [32] || onion_seed [32]` (ed25519 seed for
  the Tor onion service). Version 1 (`version u8 = 1 || scalar [32]`) is accepted and upgraded.
- History (optional): `history/index` holds `{next, count[]}`; `history/seg-N` holds a JSON
  array of `{p, i, m, t, a}` records (peer hex, msg id, mine, text, unix seconds).
- mDNS announcement (opt-in): PTR `_burrow._tcp.local` → `<random>._burrow._tcp.local`, SRV
  with the port, TXT `n=<nonce hex>` and `t=<tag hex>` where
  `tag = BLAKE2b-256("burrow/1 mdns" || nonce || pubkey)[:16]`.

## 9. Constants

Generated from `internal/wire` by `go test ./internal/wire -run TestProtocolDocConstants -update`;
`make docs-check` fails if this table drifts.

<!-- constants:begin -->
| Constant | Value |
|---|---|
| `ADSize` | 28 |
| `AcceptPromptTimeout` | 10m0s |
| `AckDelivered` | 1 |
| `BusyRejectWindow` | 10m0s |
| `ByeGracePeriod` | 2s |
| `ByeLocalError` | 2 |
| `ByeReplaced` | 3 |
| `ByeResourceLimit` | 4 |
| `ByeShutdown` | 1 |
| `ByeUserQuit` | 0 |
| `CancelHashDiverged` | 1 |
| `CancelLocalIO` | 2 |
| `CancelShutdown` | 3 |
| `CancelUser` | 0 |
| `ChainKeySize` | 32 |
| `ChainsInfo` | "burrow/1 chains" |
| `ChatQueueCap` | 64 |
| `ChunkData` | 65524 |
| `ClassChat` | 2 |
| `ClassControl` | 1 |
| `ClassTransfer` | 3 |
| `ClassUnknown` | 0 |
| `ClosedStreamMemory` | 64 |
| `ControlQueueCap` | 8 |
| `ControlQueueStall` | 5s |
| `ControllerTick` | 1s |
| `DeadPeerTimeout` | 1m30s |
| `DefaultInviteTTL` | 1h0m0s |
| `DefaultListenPort` | 47337 |
| `DefaultMaxFile` | 104857600 |
| `DefaultMaxImage` | 26214400 |
| `DirInitiatorToResponder` | 0 |
| `DirResponderToInitiator` | 1 |
| `EpochMaxFrames` | 65536 |
| `FeatureAnimatedGIF` | 4 |
| `FeatureFiles` | 8 |
| `FeatureImages` | 1 |
| `FeatureMask` | 15 |
| `FeatureTyping` | 2 |
| `FormatGIF` | 4 |
| `FormatJPEG` | 2 |
| `FormatPNG` | 1 |
| `FormatWebP` | 3 |
| `FrameADPrefix` | "burrow-frame-v1" |
| `FreeSpaceMargin` | 67108864 |
| `GlareWindow` | 5s |
| `HS1Len` | 1232 |
| `HS1PayloadLen` | 1184 |
| `HS2Len` | 1168 |
| `HS2PayloadLen` | 1120 |
| `HS3Len` | 113 |
| `HS3PayloadLen` | 49 |
| `HSLenPrefix` | 2 |
| `HSRandomSize` | 32 |
| `HandshakeMsg1Wait` | 5s |
| `HandshakeTimeout` | 10s |
| `HashSize` | 32 |
| `InnerHeaderSize` | 8 |
| `InviteTokenSz` | 16 |
| `KEMCiphertextSize` | 1088 |
| `KEMEncapsulationKeySize` | 1184 |
| `KEMSeedSize` | 64 |
| `KEMSharedSecretSize` | 32 |
| `LargeBucket` | 4096 |
| `MaxActiveTransfers` | 2 |
| `MaxBusyRejects` | 32 |
| `MaxCaptionBytes` | 1024 |
| `MaxCiphertext` | 65552 |
| `MaxConcurrentDecode` | 2 |
| `MaxContacts` | 1024 |
| `MaxContactsPerInvite` | 64 |
| `MaxDrainingStreams` | 16 |
| `MaxFileExt` | 8 |
| `MaxGIFFrames` | 200 |
| `MaxHelloName` | 32 |
| `MaxImageDecodeBytes` | 268435456 |
| `MaxImagePixels` | 40000000 |
| `MaxImageSide` | 16384 |
| `MaxInner` | 65536 |
| `MaxOffersPerWindow` | 16 |
| `MaxOuterFrame` | 65556 |
| `MaxPayload` | 65528 |
| `MaxPeers` | 32 |
| `MaxPendingHandshakes` | 64 |
| `MaxPendingOffers` | 4 |
| `MaxPingsPerWindow` | 4 |
| `MaxQueuedMessages` | 1000 |
| `MaxTextBytes` | 16384 |
| `MaxTransferSize` | 1099511627776 |
| `MaxTypingPerWindow` | 32 |
| `MinCiphertext` | 272 |
| `MinInner` | 256 |
| `Msg1ReplayLRU` | 4096 |
| `MsgIDDedupSet` | 4096 |
| `NonceSize` | 12 |
| `OfferWindow` | 1m0s |
| `OuterLenSize` | 4 |
| `PingFloodWindow` | 10s |
| `PingInterval` | 30s |
| `Prologue` | "burrow/1" |
| `ProtocolVersion` | 1 |
| `RateLimitPenalty` | 10m0s |
| `ReconnectMaxBackoff` | 1m0s |
| `ReconnectMinBackoff` | 1s |
| `RejectBusy` | 3 |
| `RejectDeclined` | 0 |
| `RejectTooLarge` | 1 |
| `RejectUnsupported` | 2 |
| `RekeyEpochFailSafe` | 25m0s |
| `RekeyInFlightTimeout` | 30s |
| `RekeyInitCounter` | 1024 |
| `RekeyInitInterval` | 15m0s |
| `RekeyRequestCounter` | 2048 |
| `RekeyRequestInterval` | 20m0s |
| `RekeyRequestTimeout` | 10s |
| `ResultAborted` | 2 |
| `ResultHashMismatch` | 1 |
| `ResultOK` | 0 |
| `RootKeySize` | 32 |
| `SmallBucket` | 256 |
| `SmallBucketMax` | 4096 |
| `StreamChat` | 1 |
| `StreamControl` | 0 |
| `StreamMinTransfer` | 2 |
| `TCPFailuresPerMinute` | 10 |
| `TagSize` | 16 |
| `TorFailuresPerMinute` | 60 |
| `TorLimitedSlots` | 16 |
| `TransferQueueCap` | 2 |
| `TypeAck` | 0x11 |
| `TypeBye` | 0x04 |
| `TypeFileOffer` | 0x27 |
| `TypeHello` | 0x01 |
| `TypeImgAccept` | 0x21 |
| `TypeImgCancel` | 0x26 |
| `TypeImgChunk` | 0x23 |
| `TypeImgDone` | 0x24 |
| `TypeImgOffer` | 0x20 |
| `TypeImgReject` | 0x22 |
| `TypeImgResult` | 0x25 |
| `TypePing` | 0x02 |
| `TypePong` | 0x03 |
| `TypeRekeyDone` | 0x32 |
| `TypeRekeyInit` | 0x30 |
| `TypeRekeyRequest` | 0x33 |
| `TypeRekeyResp` | 0x31 |
| `TypeText` | 0x10 |
| `TypeTyping` | 0x12 |
| `TypingStart` | 1 |
| `TypingStop` | 0 |
| `TypingWindow` | 1m0s |
| `WriteDeadline` | 30s |
| `X25519Size` | 32 |
<!-- constants:end -->
