# Threat model

Kept in sync with CLAUDE.md §15. Any change to a security property, default or primitive
updates this file in the same commit.

## Assets

Message and image content; the long-term identity key; the contact list and issued
invites; the fact that two specific peers talk, and when; partial transfers on disk.

## Adversaries and what we do about them

| Adversary | Protection | Status |
|---|---|---|
| Passive network observer, including one recording for a future quantum computer | Hybrid root key: X25519 (Noise XK) **and** ML-KEM-768 both feed `root_0`; per-message keys from one-way chains | done |
| Active MITM | Noise XK with pinned static keys; safety-number verification; a key that does not match a contact is a hard failure; no key-change prompts | done |
| Compromise of a long-term key after a conversation | Fresh ephemeral X25519 + ML-KEM per handshake; per-message chain keys deleted after use | done |
| Compromise of running session state (not the identity key) | Periodic rekey mixes fresh DH + KEM **and** DHs against both static keys; the attacker is evicted at the next epoch even if active | done |
| Scanners that do not hold a peer's public key | XK: silent close after one cheap DH; responder key never sent; msg1 replay LRU; invite tokens for unknown initiators | done |
| Image metadata leakage | EXIF/XMP/ICC/comments stripped; no filenames or timestamps on the wire; opaque file names | Phase 2 |
| Disk theft | Identity, contacts, invites, transfer metadata encrypted under an Argon2id-derived key (or a random key in `--insecure-no-passphrase` mode); no history by default; no core dumps (Linux/macOS) | Phase 0/1 |
| Terminal/UI injection and name spoofing | Sanitization of every peer string (`internal/text`); NFKC + case-fold normalization for the name-collision check | Phase 0 |
| Memory exhaustion, decompression bombs | Hard caps before allocation (frame ≤ 65,552 bytes, rejected before the body is read); bounded queues; per-peer stream/offer caps; image dimension gates; decode semaphore | Phase 0 (wire), Phase 2 (media) |
| A second local process corrupting the store | Exclusive lock on `store/lock` | Phase 0 |

## Out of scope (stated in the README)

- A compromised endpoint, including an attacker holding both the identity key and session
  state.
- A malicious peer sharing what they legitimately received.
- Presence detection by anyone who holds a peer's public key (every past invite recipient,
  including blocked contacts): they receive msg2 instead of silence. A recorded msg1 does
  the same until the responder restarts. Rotating identity is the only remedy.
- Protocol fingerprinting: fixed handshake sizes (1232/1168/113) and the 256/4096 frame
  buckets identify Burrow to DPI.
- Traffic analysis by a single network observer: timing, coarse sizes, rekey patterns
  (1296/1296/272-byte ciphertexts), TYPING cadence when enabled, reconnect beacons (only after a
  lost connection, never after a deliberate BYE; see SECURITY.md).
- Quantum de-anonymization of recorded handshakes: the initiator's identity and token in
  msg3 are protected only by X25519 (who talked, not what was said).
- Quantum attacks on authentication in *new* sessions (revisit with ML-DSA in v2).
- IP exposure to the peer in direct-TCP mode (use Tor, Phase 4).
- Encoder fingerprints inside images in `strip` mode (`paranoid` mode removes them).
- Store rollback from backups resurrecting a consumed single-use invite token.
- Perfect memory zeroization under Go's GC or inside library objects; secrets in swap.
- Denial of service beyond rate limiting and hard caps.
