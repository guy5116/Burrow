# CLAUDE.md — Burrow

> **Read this entire file before writing any code.** It is the single source of truth for
> this project. It is long on purpose: the protocol must stay in context so the
> implementation cannot drift from it. If context budget ever becomes a problem, move
> §3–§7, §15 and §18 verbatim into `docs/SPEC.md` and import it from here with
> `@docs/SPEC.md` — never summarize it.
>
> When something here is **contradictory or looks wrong on a security-relevant point,
> stop and ask the user**. When something is merely unspecified, choose the more
> conservative option, note it in the current `docs/phases/PHASE-N.md`, and carry on.
>
> **Every session starts by reading `docs/STATUS.md`** (§1.2) and continues from its
> "next step". If it does not exist yet, this is Phase 0's first task: create it.

---

## 0. What we are building

**Burrow** is a **peer-to-peer, end-to-end-encrypted, one-to-one chat program in Go** that
sends text, images and other files with **no servers, no accounts, and no third parties**. The name is
"Burrow" in prose and documentation, `burrow` in code, paths, and binaries. It ships as:

- `burrow` — a CLI with a full terminal UI (TUI) and a plain line-mode for scripting.
- `burrow-gui` — a native desktop GUI.

Both binaries are thin shells over one shared engine. All chat logic, crypto, networking,
and media handling live in the engine and are tested there. One process uses the store at
a time (§7); the GUI and the CLI are alternatives, not companions.

**Speed matters. Privacy is the point.** Both are hard requirements, but when they
genuinely conflict, privacy wins. A slow implementation of a privacy feature is a bug to
fix, not a trade-off to accept.

### 0.1 Priority order (higher always wins)

1. **Privacy and security** — confidentiality, authenticity, forward secrecy,
   post-compromise security, metadata minimization, safe defaults.
2. **Correctness and stability** — no crashes, no goroutine leaks, no undefined states,
   graceful recovery from network failure.
3. **Performance** — low latency, high throughput, no frame-sized allocations in the hot
   path, small memory footprint.
4. **Features** — images, terminal image rendering, Tor transport, etc.
5. **Polish** — themes, animations, nice-to-haves.

### 0.2 Scope

- **In scope (v1):** one-to-one text + images + files (§6.5) with many simultaneous peers, CLI/TUI + GUI,
  direct TCP transport, Tor onion transport (Phase 4), Linux/macOS/Windows.
- **Non-goals (v1):** group chats, voice/video, cross-device history sync, mobile apps,
  federation, a relay/discovery service of any kind, Sixel output. Do not build toward these.

### 0.3 Placeholders to confirm with the user before Phase 0

- Go module path: `github.com/guy5116/burrow` (the name Burrow itself is settled).
- Default listen port: `47337` (fixed so invites and reconnects work; user-configurable).
- Bubble Tea major version (v1 at `github.com/charmbracelet/...` or v2, whose module path
  may differ — verify with `go list -m` and record the real path in §14).

---

## 1. Working in this repo (rules for Claude Code)

### 1.1 Commands

```
make tools        # go install pinned versions of staticcheck, gosec, govulncheck, golangci-lint
make build        # go build ./cmd/burrow  (no gui tag; CGO not required)
make build-gui    # go build -tags gui ./cmd/burrow-gui  (needs CGo + OpenGL/X11 dev packages)
make cross        # GOOS=linux/darwin/windows CGO_ENABLED=0 go build ./cmd/burrow  (CLI only)
make test         # go test -race -count=1 ./...            (gui packages excluded by build tag)
make test-short   # go test -race -short ./...              (skips slow integration/argon2 tests)
make test-gui     # go test -race -tags gui ./internal/ui/gui/...
make lint         # gofmt -l, go vet, staticcheck, gosec, govulncheck, golangci-lint (config: .golangci.yml, includes depguard)
make fuzz         # runs every Fuzz* target for $(FUZZTIME), default 60s (nightly CI: FUZZTIME=10m)
make bench        # benchmarks in internal/session internal/media internal/handshake; compares to bench/baseline.json
make release      # reproducible CLI builds: -trimpath -ldflags="-s -w -buildid=" for linux/darwin/windows + SHA-256 sums
make docs-check   # verifies every constant in docs/PROTOCOL.md matches internal/wire

go test -race -run TestName ./internal/session/               # one test
go test -fuzz=FuzzOuterFrame -fuzztime=60s ./internal/wire/   # one fuzz target
```

Create the `Makefile` and `.golangci.yml` in Phase 0 with exactly these targets. `make lint`
and `make test` must pass before any task is considered done. `make test` runs with the
default CGO setting because the race runtime needs it; the **release** CLI binary is
`CGO_ENABLED=0` and `make cross` proves it builds that way on every OS.

All GUI code (`internal/ui/gui`, `cmd/burrow-gui`) carries `//go:build gui` so the engine
can be built and tested in any sandbox without graphics libraries. Each gui package also
has an untagged `doc.go` containing only the package clause, so `go vet ./...` and the
linters never see "build constraints exclude all Go files".

### 1.2 Workflow

- **Check current APIs, don't assume.** Before Phase 0 and whenever you touch them, read
  the current docs (`go doc`, module READMEs) for `crypto/mlkem`, `crypto/hkdf`,
  `github.com/flynn/noise`, Fyne, and Bubble Tea. Library APIs move; this file names
  functions as they existed when it was written.
- **Tests first for protocol and crypto code.** Write the test (known-answer or
  behavioral), watch it fail, then implement.
- Run `make test` and `make lint` before declaring any task complete. Never say something
  works if you have not run it.
- Small, focused commits with conventional messages (`feat:`, `fix:`, `test:`, `docs:`,
  `refactor:`, `perf:`, `sec:`).
- Any change to bytes on the wire → update `docs/PROTOCOL.md` in the same commit and run
  `make docs-check`.
- Any change to a security property, default, or primitive → update `docs/THREAT_MODEL.md`
  and `docs/SECURITY.md` in the same commit.
- Keep `docs/DEPENDENCIES.md` current: every third-party module, why it is needed, and what
  it is allowed to touch.
- **Keep `docs/STATUS.md` current — update it before every commit and before stopping for
  any reason.** It holds, in this order: the current phase; what is done; what is
  mid-flight (file, function, and what state it is in); the exact next step; open
  questions for the user. Keep it under 60 lines — it is a bookmark, not a log. **At the
  start of every session, read `docs/STATUS.md` first and continue from its "next step"**
  rather than re-deriving state from the code. If it disagrees with `git log` or the
  tests, trust the tests, fix STATUS.md, and say so.
- When you finish a phase (§16), write a short `docs/phases/PHASE-N.md` summarizing what
  was built, what was deferred, every unspecified choice you made, and known issues, and
  reset `docs/STATUS.md` to the next phase.
- `go.mod`: `go 1.26` minimum (`crypto/mlkem` and `crypto/hkdf` need 1.24; the UI
  dependencies need 1.26 — decided by the user on 2026-09-29) plus a `toolchain`
  directive pinning the current stable release.

### 1.3 Never

- Never implement your own cryptographic primitives. Compose the audited primitives named in
  §3 exactly as specified. No "clever" shortcuts. Hand-rolling HMAC from SHA-256 to save an
  allocation counts as implementing a primitive — don't.
- Never use `math/rand` or `math/rand/v2` for anything security-relevant: keys, nonces,
  tokens, ids, padding, salts. `crypto/rand` only. (Reconnect backoff jitter is not
  security-relevant and may use `math/rand/v2`.)
- Never log or include in an error message: keys, chain state, plaintext messages, image
  bytes, passphrases, invite strings or tokens, or peer-provided strings unredacted.
  (stdout in `--plain`/`--json` mode is the *UI channel* and carries sanitized message
  content; logs on stderr or `--log-file` never do.)
- Never `panic` on input that came from the network, a file, or the user. Parsers are
  total functions: any `[]byte` in → `(value, error)` out.
- Never use `unsafe`, `reflect`-based codecs, or code generation for anything on the wire
  path.
- Never use CGo in `internal/` outside `internal/ui/gui` (the GUI needs it via Fyne; the
  CLI release binary is `CGO_ENABLED=0`).
- Never make a network connection except to a peer the user explicitly chose (an invite the
  user pasted, or a saved contact when auto-reconnect is on). No update checks, no
  telemetry, no crash reporting, no analytics, no DNS lookups except for a hostname the
  user typed, no fetching anything referenced inside a message, no opening URLs unless the
  user turned that on and clicked.
- Never add cipher/pattern negotiation ("cipher agility"). There is exactly one suite per
  protocol major version.
- Never weaken a privacy default to make something easier.
- Never add a dependency outside the allowlist in §14 without asking.
- Never commit real keys. Test fixtures use deterministic keys generated inside tests.
- Never write raw peer bytes to a terminal or a widget. Everything peer-provided is
  sanitized (§9.5) first.
- Never block on a channel without also selecting on the session/engine context. A
  controller-initiated close must always be able to unwind every goroutine.

### 1.4 Ask the user before

- Changing any primitive, pattern, KDF, size, or constant in §3–§4.
- Adding any dependency not in §14.
- Changing a default listed as "default" anywhere in this file.
- Skipping or weakening a test category in §13.
- Deviating from the phase order in §16.

### 1.5 Definition of "done" for any task

1. `make build`, `make cross` succeed (and `make build-gui` on a machine that has the
   graphics libraries; CI does).
2. `make test` passes with `-race`.
3. `make lint` is clean.
4. New behavior has tests; new wire behavior has fuzz targets and PROTOCOL.md updates.
5. No new goroutine leaks (integration tests use `goleak`).
6. Performance budgets in §11 still hold (run `make bench` if you touched the hot path).

---

## 2. Architecture

### 2.1 Repository layout

```
cmd/burrow/                 CLI entry: subcommands, TUI, plain mode
cmd/burrow-gui/             GUI entry (Fyne)                              //go:build gui
internal/core/               Engine: sessions, peers, events, message queue (the only API UIs use)
internal/identity/           Long-term keys, fingerprints, safety numbers
internal/invite/             Invite encode/parse (needs blake2b, so not in wire)
internal/handshake/          Noise XK + hybrid ML-KEM bootstrap → root key; Authorize callback
internal/session/            Symmetric chains, frame AEAD, rekey state machine, multiplexer
internal/wire/               Byte-exact encode/decode for every frame; ALL protocol constants
internal/transport/          Transport interface; tcp/ (Phase 1), tor/ (Phase 4), mdns/ (opt-in)
internal/transport/conformance/  Shared test suite every Transport must pass
internal/media/              Image gates, metadata stripping, chunking, safe decoding
internal/store/              Master key, contacts, invites, partial-transfer metadata; encrypted-at-rest; lock
internal/text/               Sanitize, normalize, redact — used by core and both UIs
internal/secret/             Zeroizable byte buffers used by every package that touches keys
internal/buf/                Pooled buffers for the hot path
internal/ui/tui/             Bubble Tea models and views
internal/ui/gui/             Fyne windows and widgets                       //go:build gui
internal/ui/common/          Formatting shared by both UIs (no sanitization here)
internal/testpeer/           Scriptable misbehaving peer for adversarial tests
bench/                       baseline.json for make bench
docs/                        STATUS.md (read first), PROTOCOL.md, THREAT_MODEL.md, SECURITY.md, DEPENDENCIES.md, gui-test-plan.md, phases/
```

### 2.2 Dependency direction (enforced by `depguard` in golangci-lint)

```
cmd/* → internal/ui/* → internal/core → {session, handshake, identity, invite, media, store, transport} → {wire, text, secret, buf} → stdlib
```

- `internal/core` is the **only** package the UIs import. UIs never touch session, crypto,
  or transport directly.
- `internal/wire`, `internal/secret` and `internal/buf` import only the standard library;
  `internal/text` may add `golang.org/x/text`.
- `internal/session` and `internal/handshake` know nothing about images, contacts, or UI.
  They receive the two static keys (own scalar, peer public) as opaque inputs from core,
  and `handshake.Responder` takes an `Authorize(peer PeerID, token *[16]byte) Decision`
  callback supplied by core — the handshake package never sees the store.
- `golang.org/x/crypto` may be imported by identity, invite, handshake, session, store,
  media only.

### 2.3 Concurrency model

One **engine** goroutine owns peer/session bookkeeping and the event fan-out. Per connected
peer there are exactly three long-lived goroutines plus one short-lived goroutine per
image transfer in each direction:

| Goroutine | Owns | Never does |
|---|---|---|
| **reader** | the RECV chain, `root_n`, all rekey derivation, the reader's rekey sub-state, the receive side of every stream | encrypt, touch the socket for writing |
| **writer** | the SEND chain; the initiator's `rekeyInFlight` flag and rekey-ephemeral generation; the responder's `requestSentAt`; three bounded outbound queues (control cap 8, chat cap 64, per-transfer cap 2) drained with strict priority control > chat > transfer, transfers round-robined | decrypt, derive keys |
| **controller** | timers only: rekey schedule, PING schedule, REQUEST timeout, time-based fail-safes | touch any key material |
| **file-writer** (per incoming transfer) | writes chunks to the `.part` file, hashes, updates `.meta`, fsyncs | anything else |
| **file-reader** (per outgoing transfer) | both stripping passes (§6.2), disk reads, feeds that transfer's queue | anything else |

Rules:

- **Frames are encrypted only by the writer, at the instant they are written to the
  socket, with the writer's SEND chain at that instant. Nothing upstream of the writer
  ever holds ciphertext. The reader decrypts with its RECV chain at the instant it reads
  the frame.** The epoch switch points in §3.5 are defined in terms of these instants.
- Shared state is exactly this list, published with `sync/atomic` and read-only for
  everyone but its owner: `sendCounter`, `sendSwitchedAt`, `lastWrittenAt`, `pingNonce`
  (nonce of the most recently written PING; 0 = none), `rekeyInFlight` + `rekeyStartedAt`
  (initiator side), `requestSentAt` (responder side) — all owned by the **writer**;
  `recvCounter`, `recvSwitchedAt`, `pongNonce` (nonce of the most recently accepted PONG;
  0 = none), `readerRekeyState` ∈ {Idle, AwaitingDone} + `readerStateSince` (responder
  side) — owned by the **reader**. PING nonces come from `crypto/rand` and are redrawn if 0.
  Per-stream state (id allocation, open/draining/closed, the last 64 closed ids) is one
  mutex-guarded table shared by reader, writer and core; it holds no key material. Nothing
  else is shared; there is no synchronous request/response between goroutines anywhere.
  The initiator's reader has no rekey state of its own: "awaiting RESP" is exactly
  `rekeyInFlight && pending non-empty`.
- `pending` is the only other cross-goroutine channel: writer → reader, capacity 1,
  carrying the initiator's rekey ephemerals `{e_i', kem_seed'}` (§3.5). The reader drains
  it with a non-blocking receive; "non-empty" means that receive succeeds.
- Commands travel one way, as entries on the writer's control queue: `StartRekey` (from
  the controller's schedule, or from the reader on REKEY_REQUEST), `SendPing` and
  `SendRekeyRequest` (controller), and reply frames the reader must send (PONG,
  `{REKEY_RESP, newSendChain}`, `{REKEY_DONE, newSendChain}`, IMG_CANCEL echoes). If the control queue stays full for
  5 s the writer is stalled → close the session. A peer that sends more than 4 PINGs in
  10 s → close. Under these rules the control queue cannot fill during honest operation.
- Every blocking operation takes a `context.Context` and has a deadline; timeouts use
  monotonic time.
- Every channel that carries peer-influenced volume is bounded → backpressure (which flows
  back to the peer through TCP), not memory growth. A slow UI must never cause unbounded
  buffering: drop TYPING first, then apply backpressure to image chunks; never drop TEXT.
- The `Events()` channel has one consumer. A stalled consumer stalls the engine and every
  reader (head-of-line, by design). UIs therefore drain it on a dedicated goroutine, never
  on a render thread.
- All goroutines are started through a small supervisor helper that records a name, wires
  cancellation, and on exit converts a panic (always a bug) into a redacted log line plus
  clean teardown of *that session only*. On teardown the supervisor drains `pending` and the
  control queue and clears every secret they carry before buffers return to the pool. The
  process stays alive.

---

## 3. Cryptographic design

### 3.1 Primitives (fixed for protocol major version 1)

| Purpose | Primitive | Package |
|---|---|---|
| Handshake | Noise `XK_25519_ChaChaPoly_BLAKE2s` | `github.com/flynn/noise` |
| Long-term identity | X25519 static key (the Noise static key), held as a raw 32-byte scalar | `golang.org/x/crypto/curve25519` (what flynn/noise uses) |
| Rekey DH | X25519, raw scalars we own and clear | `golang.org/x/crypto/curve25519` |
| Post-quantum KEM | ML-KEM-768 (FIPS 203) | `crypto/mlkem` (Go ≥ 1.24) |
| Key derivation | HKDF-SHA-256; HMAC-SHA-256 chains | `crypto/hkdf`, `crypto/hmac`, `crypto/sha256` |
| Frame encryption | ChaCha20-Poly1305 (IETF, 12-byte nonce) | `golang.org/x/crypto/chacha20poly1305` |
| At-rest encryption | XChaCha20-Poly1305 | `golang.org/x/crypto/chacha20poly1305` (`NewX`) |
| Passphrase KDF | Argon2id, t=3, m=64 MiB, p=4, 16-byte salt, 32-byte output | `golang.org/x/crypto/argon2` |
| Hashing (files, safety numbers, invites, mDNS) | BLAKE2b-256 / BLAKE2b-512 | `golang.org/x/crypto/blake2b` |
| Randomness | `crypto/rand` only | stdlib |
| Constant-time compare | `crypto/subtle.ConstantTimeCompare` | stdlib |

Rationale, briefly: X25519 + ChaCha20-Poly1305 are constant-time in pure Go on every
platform (no AES-NI dependency). ML-KEM-768 is the NIST standard at security level 3 and
ships in the Go standard library. Noise XK gives mutual authentication, never transmits the
responder's identity, and sends the initiator's identity only under forward-secret keys.
The **hybrid** design below means an attacker must break *both* X25519 and ML-KEM to read
traffic. `crypto/ecdh` is not used directly: we hold raw 32-byte scalars we can zero; both
flynn/noise and `x/crypto/curve25519` make transient, non-clearable copies internally
(documented in `SECURITY.md`).

### 3.2 Security properties we deliver (each has a test)

| Property | Mechanism |
|---|---|
| Confidentiality against passive observers, including future quantum computers ("harvest now, decrypt later") | Hybrid root: X25519 (Noise) **and** ML-KEM-768 secrets both feed the root key |
| Mutual authentication / MITM resistance | Noise XK with pinned static keys; out-of-band safety-number verification; a key that does not match a contact is a hard failure |
| Responder not enumerable by key-less scanners | XK: a connection from anyone who does not hold the responder's public key gets silently dropped after one cheap DH; the responder's key is never transmitted; replayed msg1 rejected |
| Forward secrecy per session | Fresh ephemeral X25519 + fresh ML-KEM keypair every handshake; ephemerals zeroized after use |
| Forward secrecy per message | One-way HMAC chains; each message key deleted after use |
| Post-compromise security | Periodic in-session rekey mixes fresh ephemeral DH + ML-KEM **and DHs against both long-term keys**, so an attacker holding session state but not a long-term private key is evicted at the next rekey even if active (§3.5) |
| Replay / reorder / truncation resistance | Ordered transport + per-epoch counters in AEAD associated data + AEAD tags; any failure terminates the session |
| Downgrade resistance | Protocol version in the Noise prologue; no negotiation |
| Traffic-shape reduction | Bucket padding on every frame (§4.2); keepalives look like small messages |
| At-rest protection | Identity, contacts, invites, transfer metadata encrypted with a passphrase-derived key; no message history by default |

### 3.3 Identity, fingerprints, verification

- An identity is one X25519 keypair: 32 random bytes from `crypto/rand` as the scalar,
  public key = `curve25519.X25519(scalar, curve25519.Basepoint)`. Held in an
  `internal/secret` buffer; handed to flynn/noise as `noise.DHKey`.
- `PeerID` **is** the 32-byte public key. Everything (contacts, events, dedup) is keyed by it.
- **Fingerprint** = the public key encoded as lowercase RFC 4648 base32 without padding
  (52 characters), displayed in groups of 4. The key *is* the identity; no hashing.
- **Safety number** (for verbal/visual verification between two specific peers):
  `BLAKE2b-512("burrow/1 safety" || min(pkA, pkB) || max(pkA, pkB))` → take the first 60
  bytes as twelve 5-byte big-endian blocks → each `mod 100000` → twelve 5-digit groups.
  Both UIs render this identically; exact layout in `docs/PROTOCOL.md`.
- The identity is **encrypted at rest** (§7). A passphrase is required at creation;
  `--insecure-no-passphrase` (§7 mode 2) is allowed and prints a loud warning.
- Contacts store: `PeerID`, user-chosen nickname, `verified bool`, `blocked bool`, last
  known address(es), first-seen time, originating invite id. **A contact is created on
  either side only after both HELLOs validate.** Default nickname: the sanitized HELLO name
  if non-empty, else the first 8 fingerprint characters. **Nicknames are never used for
  trust decisions.** At most 1,024 contacts.
- **Keys are identity and never change.** If someone regenerates their identity they are a
  new contact and need a new invite. There is no "key changed, accept?" prompt anywhere.
  Consequences:
  - An initiator dialing a contact whose responder no longer holds the pinned key sees a
    silent close, which is indistinguishable from offline/blocked/firewalled. The engine
    emits `HandshakeFailed{Stage, Reason}` and the UI wording is "Could not establish a
    secure session with <nick>: they may be offline, may have blocked you, or their key may
    have changed (a fresh invite would be needed)." Never auto-accept, never fall back.
  - Whichever side creates a contact runs the **name-collision check**: if the new peer's
    HELLO name **normalizes** (§9.5) to the same string as an existing contact's nickname
    but the key differs → `NameCollision` event, both UIs warn with both fingerprints, and
    the new contact's default nickname becomes `<name> (<fp8>)`.
- Verification is an explicit step: both users compare the safety number out-of-band and
  mark the contact `verified`. Unverified peers are shown with a clear, persistent marker;
  no UI ever shows a "secure"/green indicator for an unverified peer.

### 3.4 Handshake and root key derivation

Pattern: **Noise XK**. The initiator must already know the responder's static public key
(from an invite or the contact list). The responder's static key is never sent.

```
Prologue  = "burrow/1"          (protocol major version; any change = incompatible)
Suite     = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
Config    = noise.Config{CipherSuite: Suite, Pattern: noise.HandshakeXK, Initiator: <bool>, Random: rand.Reader,
                         Prologue, StaticKeypair, EphemeralKeypair: e (generated by us, §3.6), PeerStatic (initiator only)}

Initiator                                              Responder
  gen e_i ; kem_seed = 64 random bytes ; kem_dk = mlkem.NewDecapsulationKey768(kem_seed)
  msg1: -> e, es        payload HS1 { kem_ek [1184] }                         (1232 bytes on the wire)
                                                         e_i.pub already in the msg1-replay LRU (last 4096 seen)? → close before any DH
                                                         ReadMessage verifies msg1 (one DH + AEAD); failure → close
                                                         kem_ek = mlkem.NewEncapsulationKey768(...) ; error → close
                                                         gen e_r ; (kem_ss, kem_ct) = kem_ek.Encapsulate()
                                                         r_r = 32 random bytes
  msg2: <- e, ee        payload HS2 { kem_ct [1088], r_r [32] }              (1168 bytes on the wire)
  kem_ss = kem_dk.Decapsulate(kem_ct)
  r_i = 32 random bytes
  msg3: -> s, se        payload HS3 { r_i [32], has_token u8, token [16] (zeros if none) }   (113 bytes on the wire)
                                                         Authorize(PeerStatic(), token):
                                                           known contact, not blocked → ok (a present token is ignored, never consumed)
                                                           unknown, valid unexpired invite token, caps not exceeded → ok, token RESERVED (§4.6)
                                                           otherwise → close TCP silently
h = hs.ChannelBinding()

Both:
  ikm      = r_i || r_r || kem_ss                                     (96 bytes)
  root_0   = hkdf.Extract(sha256.New, ikm, salt = h)
  ck_i2r_0 || ck_r2i_0 = hkdf.Expand(sha256.New, root_0, info = "burrow/1 chains" || uint32(0), 64)
  clear: e_i/e_r scalars, kem_seed, kem_ss, r_i, r_r, ikm; drop the HandshakeState and both CipherStates
```

Rules that must be reflected in code and tests:

- Each Noise handshake message is framed on the wire as `u16 BE length || message`, and
  **every handshake message has exactly one legal length: 1232, 1168, 113.** Any other
  length → close without writing anything. (HS3 is fixed-size on purpose.)
- The Noise CipherStates returned after msg3 are **not** used for data. All traffic uses
  the chains derived above. Their secrecy is equivalent (r_i and r_r travel only inside
  Noise-encrypted, forward-secret payloads), and this puts the PQ secret in the root.
- `h` as salt binds the whole transcript. `r_i` and `r_r` make the root contributory from
  both sides even if one side's KEM randomness were weak.
- Parsing `kem_ek` may fail (invalid encoding) → close silently. Decapsulation of a
  well-formed ciphertext never fails (implicit rejection); a mismatch is caught by the
  HELLO exchange (§4.4), which is the key confirmation. No application data may be sent
  before both HELLOs are received and validated.
- **Cost ordering for the responder:** the replay LRU is checked first (a map lookup;
  128 KiB per process); nothing is generated or encapsulated until msg1's AEAD tag
  verifies. A key-less probe costs one X25519 and one failed AEAD open.
- Handshake deadline 10 s total; msg1 must arrive within 5 s of accept. At most 64 pending
  handshakes per listener. Rate limiting is **per transport** and counts **failures**
  (successful handshakes never consume budget): TCP keys by /24 (IPv4) or /48 (IPv6) with
  10 failures/minute, then that key is accept-and-close for 10 minutes; Tor has no source
  address, so the limit is global: 60 failures/minute, then 16 slots only for 10 minutes.
  On any failure the responder closes the TCP connection **without writing anything**.
- The Noise library performs the handshake DHs per the Noise spec (its DH rejects low-order
  results). For the rekey DHs, which we compute ourselves, any error from
  `curve25519.X25519` (all-zero result) is fatal to the session.

### 3.5 Session keys, chains, and rekeying

Each direction has its own chain. Every frame (including PING/PONG/ACK/TYPING) advances
its chain.

```
mk   = HMAC-SHA256(ck, 0x01)      // 32-byte single-use message key
ck'  = HMAC-SHA256(ck, 0x02)      // next chain key; old ck and mk cleared after use
```

Frame AEAD: ChaCha20-Poly1305 with `key = mk`, `nonce = 0x00000000 || counter(u64 BE)`,
`ad = "burrow-frame-v1" || epoch(u32 BE) || counter(u64 BE) || direction(u8)` where
direction is `0` for initiator→responder and `1` for responder→initiator. Each chain has
its own `counter`, starting at 0 in every epoch and never transmitted; both sides count.
(The key is already single-use; the counter nonce is belt-and-braces against chain bugs.)
The reader clears the plaintext region of its buffer once a frame has been consumed.
**At `counter == 65,536` the writer refuses to encrypt and the reader refuses to decrypt:
synchronous close.** That is the hard epoch-length cap.

**Rekey (asymmetric ratchet with static-key binding).** Only the handshake initiator drives
rekeys; the responder can request one. The transport is ordered and reliable, so no
skipped-key storage is needed and the epoch switch is deterministic. Combined with §2.3's
encrypt-at-write rule:

```
Initiator (I)                                                  Responder (R)
  writer: on StartRekey with rekeyInFlight clear:
          set rekeyInFlight and rekeyStartedAt ; gen e_i' scalar ; kem_seed' ; kem_dk' ; push {e_i', kem_seed'} to `pending` (cap 1; full → bug → close)
  writer: REKEY_INIT { e_i'.pub [32], kem_ek' [1184] }   under epoch n
                                                                 reader: readerRekeyState must be Idle, else close
                                                                 reader: gen e_r' ; (kem_ss', kem_ct') = Encaps(kem_ek')
                                                                 reader: derive epoch n+1 ; readerRekeyState := AwaitingDone
                                                                 reader → writer control queue: {REKEY_RESP, newSendChain}
                                                                 writer: REKEY_RESP { e_r'.pub [32], kem_ct' [1088] }   under epoch n
                                                                 writer: SEND chain := n+1 immediately after writing RESP ; clear requestSentAt
  reader: rekeyInFlight must be set and `pending` non-empty, else close ; take {e_i', kem_seed'} from `pending`
  reader: derive epoch n+1 ; RECV chain := n+1 immediately after decrypting RESP
  reader → writer control queue: {REKEY_DONE, newSendChain}
  writer: REKEY_DONE {}   under epoch n  (the last epoch-n frame from I)
  writer: SEND chain := n+1 immediately after writing DONE ; clear rekeyInFlight
                                                                 reader: readerRekeyState must be AwaitingDone ; RECV chain := n+1 immediately after decrypting DONE ; readerRekeyState := Idle

Derivation (done by the reader, which owns root_n; s = long-term static keys, e' = rekey ephemerals):
  I computes: dh_ee = X25519(e_i', e_r'.pub)   dh_es = X25519(e_i', s_r.pub)   dh_se = X25519(s_i, e_r'.pub)
  R computes: dh_ee = X25519(e_r', e_i'.pub)   dh_es = X25519(s_r, e_i'.pub)   dh_se = X25519(e_r', s_i.pub)
  (any X25519 error → close session)
  root_{n+1} = hkdf.Extract(sha256.New, dh_ee || dh_es || dh_se || kem_ss', salt = root_n)          (128-byte ikm)
  ck_i2r || ck_r2i = hkdf.Expand(sha256.New, root_{n+1}, "burrow/1 chains" || uint32(n+1), 64)
  clear root_n, each old chain at its switch point, e' scalars, kem_seed', kem_ss', all three dh values
```

`dh_es`/`dh_se` are why an attacker who stole session state but not a long-term private key
cannot complete a rekey with either side and is evicted at the next epoch.

Both sides **may keep sending any non-rekey frame (chat, transfer, PING/PONG,
REKEY_REQUEST, BYE) at any point during a rekey**; a frame's
epoch is simply whatever the sender's SEND chain is when the writer writes it, and by the
switch points above the receiver's RECV chain always matches. A `StartRekey` that arrives
while `rekeyInFlight` is set is discarded by the writer, which is what makes the
"controller tick lands between RESP and DONE" race harmless.

**Control-frame legality** (anything else → close the session):

| Frame | Sender | Legal when |
|---|---|---|
| HELLO | either | exactly once, as that side's first frame |
| PING / PONG | either | after both HELLOs. A PONG is legal iff `nonce == pingNonce && pingNonce != pongNonce` (it answers the one outstanding PING); the reader then sets `pongNonce := nonce`. Any other PONG → close |
| REKEY_INIT | initiator only | responder's `readerRekeyState` is Idle (an outstanding REQUEST is fine) |
| REKEY_RESP | responder only | initiator's `rekeyInFlight` set and `pending` non-empty |
| REKEY_DONE | initiator only | responder's `readerRekeyState` is AwaitingDone |
| REKEY_REQUEST | responder only | any time after both HELLOs; the initiator's reader turns it into `StartRekey`, which the writer discards if `rekeyInFlight` |
| BYE | either | any time after both HELLOs (earlier → close like any other pre-HELLO frame, §4.4) |

For the receiving reader, "after both HELLOs" means "after the peer's HELLO validated":
the local HELLO is always the writer's first frame, so nothing the peer sends can honestly
precede it.

**Schedule** (constants in `internal/wire`; the controller evaluates on a 1 s tick from
the atomics in §2.3):

- Initiator: `StartRekey` when either chain counter ≥ 1024 **or** 15 minutes since
  `sendSwitchedAt`, whichever comes first.
- Responder: `SendRekeyRequest` when either counter ≥ 2048 or 20 minutes since
  `sendSwitchedAt`, `readerRekeyState` is Idle, and `requestSentAt` is unset. The writer
  sets `requestSentAt` when it writes REQUEST and clears it when it writes RESP. If
  `requestSentAt` is still set 10 s later → close.
- Time fail-safes: 25 minutes since either chain last switched, or `rekeyInFlight` for more
  than 30 s (`rekeyStartedAt`), or `readerRekeyState == AwaitingDone` for more than 30 s
  (`readerStateSince`)
  → **close the session** and clear everything. (The 65,536 counter cap above is enforced
  synchronously, not on the tick.)

**Connection loss = session loss.** Ratchet state is never persisted or resumed. A
reconnect is a brand-new handshake. Unsent messages stay in an in-memory queue with
status `pending` (§12).

### 3.6 Memory hygiene

- Every secret **we own** lives in an `internal/secret` buffer and is cleared with `defer`
  as soon as it is no longer needed: identity scalar, handshake and rekey ephemeral
  scalars (generate handshake ephemerals ourselves with `Suite.GenerateKeypair(rand.Reader)`
  into a secret buffer and pass them as `Config.EphemeralKeypair`, so we hold them), ML-KEM
  seeds, `kem_ss`, `r_i`/`r_r`, every `dh`, `root`, chain keys, `mk`, the store master key,
  and anything in `pending` or the control queue at teardown.
- Library-internal copies (inside `flynn/noise` state, `x/crypto/curve25519` calls, `mlkem`
  key objects, hash states) **cannot** be cleared through their APIs. Drop the references
  promptly and say so in `docs/SECURITY.md`. Zeroization under Go's GC is best-effort;
  document that too.
- Never store secrets in strings (immutable, cannot be wiped). Use `[]byte`/fixed arrays.
- On Linux set `RLIMIT_CORE = 0` and `PR_SET_DUMPABLE = 0` at startup (via `x/sys`); on
  other OSes disable core dumps where the API exists. Swap is not prevented without
  memguard; say so.
- Optionally support `github.com/awnumar/memguard` (pure Go, `mlock`) for the identity key
  behind a build tag in Phase 5 — ask first.
- Never write secrets to temp files. Decoded images for display live in memory only until
  the widget is disposed.

---

## 4. Wire protocol (v1)

Protocol v1 had not been released when file transfer (§6.5) was added on 2026-09-29 at
the user's request, so HELLO and the frame table were amended in place instead of
starting a v2. Once a release exists, any such change is a new major version.

All integers are big-endian. All parsing is bounds-checked, allocation-free where possible,
and fuzzed. `internal/wire` is the only place that knows frame byte layouts, and it exports
every constant below by name.

### 4.1 Outer frame

```
u32   len          MinCiphertext (272) ≤ len ≤ MaxCiphertext (65,552); reject BEFORE reading the body
[]u8  ciphertext   ChaCha20-Poly1305(mk, nonce, ad, inner) — see §3.5
```

`MaxCiphertext = 65,536 (largest legal inner frame, §4.2) + 16 (tag)`. `MinCiphertext` is
a zero-payload inner frame padded to 256 plus the tag. The reader uses one reusable
`4 + MaxCiphertext` buffer per connection. The only cleartext on the wire after the
handshake is the length prefix.

### 4.2 Inner plaintext

```
u8    type
u8    flags        must be 0 in v1; any other value → close
u16   stream
u32   payload_len  ≤ 65,528
[]u8  payload
[]u8  padding      zero bytes; receiver ignores content
```

Padding rule: let `inner = 8 + payload_len`. `padded(inner)` is the **smallest multiple of
256 that is ≥ inner** when `inner ≤ 4096`, otherwise the **smallest multiple of 4096 that is
≥ inner**. A value already on a boundary is unchanged (65,536 → 65,536). `padded` never
exceeds 65,536. The receiver **enforces** the rule: a decrypted inner frame whose total
length ≠ `padded(8 + payload_len)` → close the session.

**Protocol violations close the TCP connection without sending BYE** — a BYE would tell an
attacker which probe was detected. `BYE reason=2` is reserved for local failures the peer
could not have caused (our own I/O error, say).

### 4.3 Streams

- `0` — control: `HELLO PING PONG BYE REKEY_*`
- `1` — chat: `TEXT ACK TYPING`
- `≥ 2` — one stream per transfer (an image or another file). The handshake initiator allocates even ids
  (2, 4, …), the responder odd ids (3, 5, …); ids are never reused within a session.
  Exhaustion → `BYE reason=4` and reconnect.

Stream rules (violations → close the session unless stated otherwise):

- A frame on a stream that is not open, or an `IMG_OFFER`/`FILE_OFFER` on an id of the
  wrong parity or one already used → close. An offer of a kind the receiver's own HELLO
  did not advertise (bit0 for images, bit3 for files) → close. An `IMG_CANCEL` on one of the last 64 closed stream ids is ignored;
  any other frame on a closed id → close.
- Per peer, per direction: at most **4 pending (unaccepted) offers** and **2 active
  transfers**. A 5th offer or 3rd transfer is answered with `IMG_REJECT reason=3` (busy)
  without emitting an event; more than 32 busy-rejects in 10 minutes → close. More than 16
  offers in any 60 s → close. More than 32 TYPING frames in any 60 s → drop them.
- `size ≥ 1`; `chunks = ceil(size / ChunkData)`; chunk `i` must carry index `i` exactly and
  `ChunkData` bytes, except the last which carries `size − (chunks−1)·ChunkData`.
  `start_chunk` must be `< chunks`.
- Terminal frames are `IMG_REJECT`, `IMG_RESULT`, `IMG_CANCEL`. A side that **receives**
  `IMG_CANCEL` on an open stream replies with exactly one `IMG_CANCEL` and closes the
  stream. A side that **sends** `IMG_CANCEL` first enters *draining*: it discards every
  frame on that stream until any terminal frame arrives, then closes. `IMG_REJECT` and
  `IMG_RESULT` close the stream on send and on receipt (nothing can be in flight behind
  them). At most 16 draining streams per peer; more → close.

Multiplexing policy (writer goroutine): control > chat > transfer. A chat frame never waits
behind more than one chunk. Concurrent transfers are round-robined.

### 4.4 Frame types and payloads

| Type | Name | Stream | Payload |
|---|---|---|---|
| 0x01 | HELLO | 0 | `features u64` bitmap (bit0 images, bit1 accepts TYPING, bit2 animated GIF, bit3 files; other bits must be 0 → else close), `max_image u64` (must be 0 iff bit0 is clear, else ≥ 1; else close), `max_file u64` (largest non-image file accepted, in bytes; must be 0 iff bit3 is clear; else close), `name_len u8`, `name` (≤ 32 bytes UTF-8, sanitized, **empty by default**). **No version strings** (fingerprinting). The initiator sends HELLO immediately after msg3 and then waits; the responder answers with its own HELLO and may then send data; the initiator may send data once the responder's HELLO is validated. Any other frame before both HELLOs → close. |
| 0x02 | PING | 0 | `nonce u64` |
| 0x03 | PONG | 0 | echoed `nonce u64` |
| 0x04 | BYE | 0 | `reason u8` (0 user quit, 1 shutdown, 2 local error, 3 replaced by newer session, 4 resource limit). Best-effort. |
| 0x10 | TEXT | 1 | `msg_id u64` (random from `crypto/rand`, so no message-count leakage), `sent_at i64` (unix seconds; 0 when the user disabled timestamps), `text` (UTF-8, ≤ 16,384 bytes) |
| 0x11 | ACK | 1 | `msg_id u64`, `status u8` (1 delivered). ACK for an unknown `msg_id` is ignored. |
| 0x12 | TYPING | 1 | `state u8` (0 stop, 1 start). **Off by default**; only sent if the peer's HELLO set bit1. |
| 0x20 | IMG_OFFER | ≥2 | `size u64`, `format u8` (1 PNG, 2 JPEG, 3 WebP, 4 GIF), `width u32`, `height u32`, `blake2b256 [32]`, `caption_len u16`, `caption` (≤ 1,024 bytes, sanitized, single-line). **No filename** is ever sent. Only sent if the peer's HELLO set bit0; an animated GIF is rejected locally if the peer lacks bit2. |
| 0x21 | IMG_ACCEPT | ≥2 | `start_chunk u32` (0, or a resume point) |
| 0x22 | IMG_REJECT | ≥2 | `reason u8` (0 user declined, 1 too large, 2 unsupported, 3 busy) |
| 0x23 | IMG_CHUNK | ≥2 | `index u32`, `data` — `ChunkData = 65,524` bytes for every chunk except the last (8 header + 4 index + 65,524 = 65,536, no padding) |
| 0x24 | IMG_DONE | ≥2 | empty (sender → receiver, after the last chunk) |
| 0x25 | IMG_RESULT | ≥2 | `status u8` (0 ok, 1 hash mismatch, 2 aborted) (receiver → sender) |
| 0x26 | IMG_CANCEL | ≥2 | `reason u8` (0 user, 1 sender hash divergence, 2 local I/O error, 3 shutdown; other → close) (either side) |
| 0x27 | FILE_OFFER | ≥2 | `size u64` (1 … 2^40), `blake2b256 [32]`, `ext_len u8`, `ext` (≤ 8 bytes, `a–z` and `0–9` only, may be empty; anything else → close), `caption_len u16`, `caption` (≤ 1,024 bytes, sanitized, single-line). Opens a stream like IMG_OFFER; the rest of the transfer uses the IMG_* frames. **No filename** is ever sent. Only sent if the peer's HELLO set bit3 and `size ≤` its `max_file`. |
| 0x30 | REKEY_INIT | 0 | `e_pub [32]`, `kem_ek [1184]` |
| 0x31 | REKEY_RESP | 0 | `e_pub [32]`, `kem_ct [1088]` |
| 0x32 | REKEY_DONE | 0 | empty |
| 0x33 | REKEY_REQUEST | 0 | empty |

`IMG_OFFER` and `FILE_OFFER` sizes above 2^40 bytes (1 TiB) do not decode. Unknown frame type, wrong stream for a type, or a payload that does not decode exactly →
close the session (v1 has no extensions; an extension is a new major version). The receiver
keeps the last 4096 TEXT `msg_id`s per peer in memory and drops duplicates (resends after
reconnect are idempotent); ACKs are re-sent for duplicates. The set is not persisted, so a
message ACKed just before the receiver crashed may be shown twice after a restart — accepted.

### 4.5 Keepalive and liveness

PING every 30 s of send-side silence, but never while a PING is outstanding
(`pingNonce != pongNonce`); an unanswered PING is not repeated — the 90 s rule is what
detects a dead peer. If no frame of any kind is received for 90 s the connection is dead:
close, clear, and (if the peer is a contact) schedule reconnect.

### 4.6 Invites and first contact (`internal/invite`)

Problem: the initiator must know the responder's key (XK), and the responder must decide
whether to accept an initiator it has never seen — without becoming enumerable by scanners.

- An **invite** is a bearer credential *and* reveals the responder's address and key: treat
  it like a password (never in logs, never in argv if avoidable — §9.1). Layout:
  ```
  "burrow1:" || base32-lower-nopad( version u8 = 1 || kind u8 (1 tcp, 2 tor) || addr_len u8 || addr (UTF-8: IP literal, hostname, or .onion)
                                     || port u16 || pubkey [32] || token [16] || expiry i64 (unix s) || flags u8 (bit0 multi-use)
                                     || check [4] = BLAKE2b-256("burrow/1 invite" || all preceding bytes)[:4] )
  ```
  The parser is strict (exact length, `check` must match, `addr_len ≤ 253`) and rejects
  **before any network action** — a mistyped invite must never cause a connection to the
  wrong host. Defaults: TTL 1 h, single-use. Shown as one copy-pasteable string and, in
  both UIs, as a QR code.
- The responder stores issued invites (encrypted, §7) and accepts an **unknown** initiator
  only if HS3 carries a valid, unexpired token. At msg3 the token is **reserved** (a second
  connection presenting the same single-use token while one is reserved is closed
  silently). Only after **both HELLOs validate** is the token consumed (single-use), the
  contact created with `verified = false`, and `NewPeerViaInvite` + `InviteConsumed{Id}`
  emitted (a multi-use invite emits `InviteConsumed` on every use and stays valid until
  expiry). A handshake that dies before then releases the reservation and burns nothing.
- Caps: at most **64 contacts** may be created from one multi-use invite and 1,024 contacts
  total; beyond either, the token is treated as invalid (silent close).
- A wrong or missing token from an unknown key → close silently, exactly like a wrong-key
  probe. Known, unblocked contacts need no token (`has_token = 0`); if they send one it is
  ignored. Blocked contacts are closed silently.
- Verification is separate (§3.3).

---

## 5. Transports

```go
type Transport interface {
    Listen(ctx context.Context) (Listener, error)          // Accept() → net.Conn
    Dial(ctx context.Context, addr Address) (net.Conn, error)
    Kind() Kind                                            // "tcp", "tor"
}
```

The handshake and session layers see only `net.Conn`. Every transport passes the same
`internal/transport/conformance` test suite.

- **Phase 1 — Direct TCP.** `TCP_NODELAY` on, OS keepalive on, dual-stack. One side must be
  reachable (LAN, port forward, or a VPN such as WireGuard/Tailscale). Document this
  limitation plainly in the README. Hostnames are allowed, but the UI warns that DNS reveals
  the peer's hostname to the resolver; IPs and onion addresses are preferred.
- **Phase 4 — Tor onion service (v3).** Highest-privacy mode: hides both IPs, needs no port
  forwarding, gives each user a stable address. Control a system `tor` binary via
  `github.com/cretz/bine`; never bundle Tor. The onion service's private key is a second
  long-term secret: generated with `crypto/rand`, stored **inside the encrypted identity
  blob**, handed to tor via the control port (`ADD_ONION`) at startup, never left in tor's
  data directory. The full Noise/hybrid layer still runs inside the onion connection —
  identity binding, PQ, and per-message FS are ours, not Tor's.
- **Opt-in — mDNS LAN discovery** (`golang.org/x/net/dns/dnsmessage`, hand-built packets).
  Off by default (it announces presence on the LAN). When on, an instance announces a
  random per-session service name, its port, and a TXT record `nonce[16] || tag[16]` with
  `tag = identity.MDNSTag(nonce, my_pubkey) = BLAKE2b-256("burrow/1 mdns" || nonce || my_pubkey)[:16]`,
  computed in `internal/identity` (`transport/mdns` never imports `x/crypto`). The transport
  hands `(name, port, nonce, tag)` to core; core recognizes a contact by recomputing the
  tag once per stored contact and dials at most once; anyone else learns only that *some*
  Burrow instance is on the LAN. **No probing handshakes** — they would trip the §3.4
  failure limits and reveal contact counts.
- **Explicitly rejected:** libp2p (huge dependency surface; DHT participation leaks
  presence), public STUN/TURN (third party sees metadata). UDP hole-punching via a
  user-hosted rendezvous may be revisited after Phase 5.

---

## 6. Images (`internal/media`)

Formats: PNG, JPEG, WebP (decode via `golang.org/x/image/webp` — **Go has no WebP encoder**;
animated WebP is rejected), GIF (static and animated, ≤ 200 frames). Everything else is
rejected at offer time.

### 6.1 Size gates (applied by both sender and receiver, before any decode)

- `image.DecodeConfig` only; reject if `width*height > 40,000,000`, either side > 16,384,
  or `width*height*bytesPerPixel(declared color model) > 256 MiB`.
- GIF: **pre-scan the block structure without decoding** (walk image descriptors, skip
  LZW sub-blocks) to count frames and sum frame areas; reject if frames > 200 or
  Σ(frame w×h) > 40,000,000. `gif.DecodeAll` decodes every frame, so it must never be
  called before this scan.
- A process-wide semaphore allows **2 full-size decodes at a time**. Thumbnails are
  produced once and the full-size image is released unless a viewer is open.

### 6.2 Sending pipeline (runs on the per-transfer file-reader goroutine)

1. Sniff magic bytes; reject if they disagree with the extension or are unsupported. Apply
   §6.1.
2. **Apply EXIF orientation, then strip metadata** (`strip` mode, default, lossless):
   - JPEG: keep SOI, a rewritten minimal 16-byte APP0/JFIF (no thumbnail), APP14 (Adobe
     transform flag — needed for correct colors, carries nothing identifying), DQT, DHT,
     SOF*, DRI, SOS + scan data, EOI. Drop APP1 (EXIF/XMP), APP2 (ICC), APP13
     (IPTC/Photoshop), JFXX, COM, every other APPn, and anything after EOI.
   - PNG: keep IHDR, PLTE, tRNS, gAMA, sRGB, IDAT, IEND. Drop tEXt/zTXt/iTXt/tIME/eXIf/
     iCCP/pHYs and every other ancillary chunk. Validate chunk lengths and CRCs while
     walking; recompute CRCs on output.
   - WebP: drop EXIF, XMP, ICCP chunks; clear the matching VP8X flag bits; rewrite RIFF sizes.
   - GIF: drop comment, plain-text, and application extension blocks except NETSCAPE2.0
     looping; drop anything after the trailer.
   - If orientation ≠ 1 the image is fully decoded (bounded by §6.1) and re-encoded:
     PNG → PNG, JPEG → JPEG quality 92, WebP → **PNG** (no encoder), and the offered
     `format` changes accordingly. Dropping ICC may shift colors slightly on wide-gamut
     images — document it. Strip mode leaves encoder fingerprints (quantization tables,
     zlib settings) — document it (§15).
   - `paranoid` mode (setting, default off): always decode and re-encode to PNG, discarding
     anything that is not pixels, including encoder fingerprints. Larger files. Always
     buffers the whole decoded image (bounded by §6.1).
3. Stripping is **deterministic** and runs twice so the whole file is never held in memory
   in strip mode and nothing is written to disk: pass 1 strips → streams into BLAKE2b-256
   → discards, recording `size` and the hash for the offer; pass 2 strips again while
   chunking; if the running hash diverges at the end → `IMG_CANCEL reason=1` +
   `TransferFailed`.
4. Progress events are throttled to at most 4 per second.
5. Default size limit 25 MiB; the effective limit is the min of both peers' `max_image`.

### 6.3 Receiving pipeline

1. `IMG_OFFER` → apply §6.1 to the declared dimensions and §4.3's caps; offers above the
   local limit are rejected with reason 1 before the user is asked. Otherwise emit
   `ImageOffered`. **Nothing is downloaded without an explicit accept** from the user
   (GUI/TUI prompt; plain mode `/accept <id>`). The setting `auto_accept_from_verified`
   (default off) may bypass the prompt for verified contacts. Before accepting, check free
   space ≥ `size` + 64 MiB.
2. Each transfer gets a random 16-byte `id`. Chunks are written by the file-writer
   goroutine to `<data>/partials/<id>.part` (`0600`, plaintext chunks); its sidecar
   `<data>/store/partials/<id>.meta` is an encrypted blob (§7) holding peer key, size,
   hash, format, destination directory, chunks complete — rewritten every 16 chunks and on
   pause. Nothing in a filename identifies the peer. On `IMG_DONE` verify the BLAKE2b
   hash, `fsync`, then atomically rename to `<dest>/img-<hash8>.<ext>` (`<ext>` from the
   **sniffed** format, never the sender's field; on collision add `-2`, `-3`, …) and delete
   the meta. Mismatch → delete part + meta, `IMG_RESULT status=1`.
3. Before display: sniff magic bytes again (the sender's `format` field is untrusted),
   re-apply §6.1, then decode with **stdlib decoders only** (`image/png`, `image/jpeg`,
   `image/gif`, `x/image/webp`) on a worker goroutine. Hand an `image.Image` to the UI;
   never hand raw peer bytes to a toolkit loader. The UI thread never decodes.
4. Resume: after reconnect, if a `.part` + valid `.meta` exist for the same `(peer, hash)`,
   the receiver truncates the `.part` to `complete × ChunkData`, re-hashes it from the
   start, answers the new offer with `start_chunk = complete`, and emits `TransferResumed`.
   On startup, parts without a valid meta, or whose meta names a removed contact, are
   deleted.

### 6.4 Never

- Never send the original filename, path, or timestamps.
- Never decode an image without the size gate — decompression bombs are the threat.
- Never auto-open received files with the OS.
- Never decode, display, or read into memory a received file that is not an image.

### 6.5 Files that are not images

Any regular, non-empty file can be offered (`FILE_OFFER`, §4.4): archives, documents,
anything. Added on 2026-09-29 at the user's request.

- **Size limit.** `max_file_mib` (default 100, range 0–1,048,576; **0 refuses files**: bit3
  stays clear and the peer cannot even offer one). It is independent of `max_image`. The
  limit is advertised in HELLO; the sender refuses locally, before reading the file, when
  the size exceeds the min of both peers' limits. A peer that ignores the advertised limit
  is answered with `IMG_REJECT reason=1` before the user is asked.
- **The size is shown before anything is downloaded.** Every offer prompt (TUI, plain,
  JSON, GUI, `/transfers`) states the size first, then the type. Nothing is downloaded
  without an explicit accept. `auto_accept_from_verified` applies to images only, never
  to files.
- **No name, only a type.** The offer carries the file's extension (lower-cased, `a–z0–9`,
  ≤ 8 bytes, else empty) and an optional caption. The receiver saves
  `<dest>/file-<hash8>.<ext>` (`.bin` when empty). The extension is a claim by the
  sender; the UI says so by showing it as "type".
- **No stripping.** The bytes travel as they are: metadata inside documents and archives
  (author fields, embedded file names and timestamps) is **not** removed (§15). A file
  whose magic bytes are a supported image format always takes the image pipeline (§6.2)
  instead, so a photo sent with `/file` still loses its metadata.
- **Two passes**, like images: pass 1 hashes the file for the offer, pass 2 sends it; a
  file that changed in between → `IMG_CANCEL reason=1`. Resume via `.meta` works the same.
- The §6.1 gates, decoding and thumbnails do not apply. Burrow never opens, executes,
  decodes or previews a received file.

---

## 7. Storage and local privacy (`internal/store`)

- Locations: config `$XDG_CONFIG_HOME/burrow/`, data `$XDG_DATA_HOME/burrow/`
  (`%APPDATA%\burrow` on Windows, `~/Library/Application Support/burrow` on macOS).
  Directories `0700`, files `0600` on Unix; on Windows rely on the per-user profile ACL and
  say so in `docs/SECURITY.md`.
- **One process at a time.** On unlock, take an exclusive lock on `<data>/store/lock`
  (`flock` on Unix, exclusive open on Windows); a second process fails with "store in use".
- Config is TOML (`config.toml`, nothing secret in it). Everything else lives under
  `<data>/store/`:
  - `master.hdr` (plaintext): `magic "BRWM" || version u8 || mode u8 (1 passphrase, 2 none) || argon2 t u32, m u32 (KiB), p u8 || salt[16]`.
    Parse bounds: `1 ≤ t ≤ 16`, `8 MiB ≤ m ≤ 1 GiB`, `1 ≤ p ≤ 16`; else refuse to unlock.
  - Mode 1: `master = Argon2id(passphrase, salt)` — computed **once per process**. Mode 2
    (`--insecure-no-passphrase`): `master` is a random 32-byte key stored in `master.key`
    (`0600`) beside the header; Argon2 is skipped. `burrow passphrase` converts between
    modes via the directory swap below. Either way `master` lives in an `internal/secret`
    buffer, and `debug.FreeOSMemory()` runs after unlock so Argon2's 64 MiB is returned.
  - There is no passphrase verifier: a wrong passphrase surfaces as AEAD failure on
    `identity`, reported as "wrong passphrase or corrupted store".
  - Each blob (`identity`, `contacts`, `invites`, `partials/<id>.meta`, optional history):
    `magic "BRWS" || version u8 || nonce[24] || XChaCha20-Poly1305(file_key, nonce, ad, plaintext)`
    with `ad = "BRWS" || version || nonce || relpath` and
    `file_key = hkdf.Expand(sha256.New, master, "burrow/1 store " || relpath, 32)`, where
    `relpath` is the path relative to `store/` (e.g. `partials/<id>.meta`), so a blob
    copied to another name or directory fails to open.
- All single-file writes are atomic: temp file in the same directory, `fsync`, rename,
  `fsync` the directory (Unix).
- **Passphrase change / mode change** (`burrow passphrase`) is atomic across files: write a
  complete `store.new/` (new header, `master.key` if mode 2, every blob re-encrypted, a
  fresh `lock`), fsync everything, rename
  `store/` → `store.old/`, rename `store.new/` → `store/`, delete `store.old/`. Startup
  recovery, in this order: `store.new/` and `store.old/` present but no `store/` → the new
  store was complete before the first rename, so roll forward (rename `store.new/` →
  `store/`, delete `store.old/`; the *new* passphrase applies); `store.new/` present
  alongside `store/` → delete `store.new/`; `store.old/` present alongside `store/` →
  delete `store.old/`; only `store.old/` present → rename it back. `burrow init` refuses
  to run while any of `store/`, `store.new/`, `store.old/` exists; every other command,
  once recovery has run and no `store/` exists, exits with "run `burrow init` first".
- **No message history by default.** Optional encrypted history (Phase 4) uses the same
  blob format in opaque numbered segment files with an encrypted index (filenames leak
  nothing about days); off by default; `burn` wipes it.
- Received images are saved only after explicit accept, to the configured directory
  (default `<data>/images/`, user-changeable).
- Logs go to stderr only, never to disk unless `--log-file` is given, and never contain
  content (§1.3). Peer-provided strings pass through `text.Redact()` before any log call.

---

## 8. Core engine API (`internal/core`)

The UIs are thin. If a feature needs logic, it goes in the engine with tests.

```go
type PeerID = identity.PeerID   // [32]byte: the peer's X25519 public key. Declared in internal/identity
                                // so handshake (Authorize), store and core share one type without importing core.

type Engine struct{ /* unexported */ }

func New(cfg Config, st *store.Store, tr []transport.Transport) (*Engine, error)
func (e *Engine) Start(ctx context.Context) error   // starts listeners; returns when ctx ends
func (e *Engine) Events() <-chan Event               // bounded; single consumer; drain on a dedicated goroutine

// Identity, contacts, invites
func (e *Engine) Identity() Identity                          // fingerprint etc.
func (e *Engine) CreateInvite(opts InviteOptions) (Invite, error)
func (e *Engine) Contacts() []Contact
func (e *Engine) RenameContact(id PeerID, name string) error
func (e *Engine) VerifyContact(id PeerID) error                // marks verified
func (e *Engine) BlockContact(id PeerID, blocked bool) error
func (e *Engine) RemoveContact(id PeerID) error
func (e *Engine) SafetyNumber(id PeerID) (SafetyNumber, error)

// Connections
func (e *Engine) Connect(ctx context.Context, target Target) (PeerID, error)  // parsed invite or contact id
func (e *Engine) Disconnect(id PeerID) error

// Messages
func (e *Engine) SendText(id PeerID, text string) (MsgID, error)        // queues if offline
func (e *Engine) SetTyping(id PeerID, typing bool)                      // no-op unless enabled + peer supports it
func (e *Engine) SendImage(ctx context.Context, id PeerID, path string, caption string) (TransferID, error)
func (e *Engine) SendFile(ctx context.Context, id PeerID, path string, caption string) (TransferID, error)   // any file; images are stripped (§6.5)
func (e *Engine) AcceptImage(t TransferID, destDir string) error
func (e *Engine) RejectImage(t TransferID) error
func (e *Engine) CancelTransfer(t TransferID) error
```

Events (one Go type each; both UIs use an exhaustive switch): `PeerConnected`,
`PeerDisconnected`, `Reconnecting{Attempt, NextAt}`, `HandshakeFailed{Stage, Reason}`,
`NewPeerViaInvite`, `InviteConsumed{Id}`, `NameCollision`, `PeerVerified`,
`MessageReceived`, `MessageStatus` (pending/sent/delivered/failed), `Typing`,
`ImageOffered`, `TransferProgress`, `TransferResumed`, `TransferDone`, `TransferFailed`,
`ErrorEvent` (user-facing, already redacted).

---

## 9. CLI / TUI (`cmd/burrow`, `internal/ui/tui`)

### 9.1 Subcommands

```
burrow init [--insecure-no-passphrase]   create identity (prompts for passphrase, no echo); refuses if a store exists
burrow id [--qr]                         print fingerprint
burrow invite [--ttl 1h] [--multi-use] [--qr]
burrow listen                            listen and open the TUI
burrow connect [<contact>|<invite>|-]    connect and open the TUI (also keeps listening)
                                          with no argument: prompts for an invite string (no echo); `connect -` reads it from stdin;
                                          an invite passed as an argument is accepted with a warning (argv, shell history, ps)
burrow contacts list|verify|rename|remove|block|unblock
burrow config get|set <key> [value]
burrow passphrase                        change the passphrase or mode (re-encrypts the store)
burrow burn                              wipe optional history and partial transfers
```

Global flags: `--plain` (line mode), `--json` (newline-delimited JSON events on stdout, for
scripting and tests), `--log-level`, `--log-file`, `--config`, `--data`. Passphrases and
invites are read with `golang.org/x/term.ReadPassword`, never from flags or the environment.

### 9.2 TUI

Bubble Tea (the major pinned in §0.3). Views: contact/peer list, conversation, input,
status bar (peer fingerprint short form, verified marker, transport kind). Slash commands
inside the conversation: `/image <path> [caption]`, `/file <path> [caption]`, `/accept <id>`, `/reject <id>`,
`/verify`, `/typing on|off`, `/quit`. Keyboard-only usable; keys documented in `--help`.
The TUI never opens URLs.

### 9.3 Plain mode

`--plain`: no alternate screen, no colors, reads lines from stdin, writes one event per
line to stdout. Works under pipes, `screen`/`tmux`, and screen readers. This is also what
the end-to-end tests drive (with `--json`).

### 9.4 Terminal images

Off in `--plain`. In the TUI, detect Kitty graphics protocol (`KITTY_WINDOW_ID` / `TERM`)
or iTerm2 inline images (`TERM_PROGRAM`) — both are base64 PNG and need no dependency;
render the thumbnail inline when supported; otherwise print the saved path (the app never
opens files with the OS). Sixel is out of scope for v1. The renderer receives an
`image.Image` from `internal/media`, never bytes.

### 9.5 Sanitization and normalization (`internal/text`, used by core and both UIs)

Every peer-provided string (text, caption, display name) is **sanitized** at the
session/core boundary before it can exist as a Go string:

- must be valid UTF-8 — otherwise the frame is invalid → close the session (§4.4);
- length limits from §4.4 are enforced on the byte slice before any string conversion;
- replaced with `U+FFFD`: C0/C1 controls except `\n` and `\t` (`\r` included); bidi
  overrides/isolates U+202A–U+202E, U+2066–U+2069; U+2028, U+2029; U+200B, U+200E, U+200F,
  U+2060–U+2064, U+FEFF, U+061C, U+180E; tag characters U+E0000–U+E007F;
- U+200C/U+200D (ZWNJ/ZWJ) are **kept** in TEXT and captions (emoji sequences, Persian,
  Hindi) with runs capped at 2, and removed from display names;
- runs of combining marks (categories Mn/Me) are capped at 4 per base character;
- in single-line fields (names, captions) `\n` and `\t` become a space;
- the terminal never receives peer bytes directly — only the sanitized string, and only
  through the TUI's rendering layer (never `fmt.Print` of peer content).

Display names are additionally **normalized** for the `NameCollision` check only (the
stored name stays sanitized-but-unnormalized): NFKC (`x/text/unicode/norm`) → Unicode
case-fold (`x/text/cases.Fold`) → remove every rune in `unicode.Cf ∪
unicode.Other_Default_Ignorable_Code_Point ∪ unicode.Variation_Selector` → collapse
whitespace runs to one space → trim. Compare byte-equal.

`text.Redact(s)` replaces every peer-provided string in log output with its length and a
short hash; it is the only way peer strings reach a logger.

---

## 10. GUI (`cmd/burrow-gui`, `internal/ui/gui`, build tag `gui`)

Toolkit: **Fyne v2** (pure Go except OpenGL bindings via CGo, cross-platform, ships an
image canvas). Rejected: Wails (JS frontend + webview = large surface), Gio (fine, but more
work for lists and dialogs).

### 10.1 Windows and flows

- Unlock dialog on launch (passphrase); identity creation wizard if none exists; "store in
  use" dialog if the CLI holds the lock.
- Main: contact sidebar (verified marker, online state) + conversation + composer.
- Image bubbles: thumbnail from the engine's decoded image; click → viewer with fit/1:1.
  Drag-and-drop a file onto the composer to offer it.
- Verify dialog: safety number in 12 groups, large and readable; "Mark as verified".
- `NewPeerViaInvite` / `NameCollision` / `HandshakeFailed` dialogs: show fingerprints, one
  sentence on the risk; the default button is the safe one.
- Invite dialog: QR (`github.com/skip2/go-qrcode`) + copyable string + TTL. Paste-invite
  dialog for connecting (a password-style field).
- Settings: listen port, transport, privacy toggles (typing, timestamps, auto-accept from
  verified, paranoid images, mDNS, open links), image directory, largest image and largest
  file (0 refuses files), passphrase change.
- Offer dialog for images and files: the size comes first; Reject is the default button.

### 10.2 Rules

- The UI goroutine never blocks on the network or on decoding; marshal engine events to
  the UI with Fyne's thread-safe mechanism (`fyne.Do` / `fyne.DoAndWait`, Fyne ≥ 2.6 —
  check the current API).
- Message text is rendered as plain text. No markdown, no HTML, no auto-linking. Links, if
  detected, are shown as text; clicking offers **"Copy link"** by default. "Open in browser"
  exists only behind the `open_links` setting (default off) and always shows the full URL
  in a confirmation dialog first.
- The GUI and CLI share one `store.Config` and identical defaults.
- Fyne widgets receive sanitized strings only (§9.5).

---

## 11. Performance budgets (checked by `make bench`; regressions > 15 % block merge)

| Metric | Budget |
|---|---|
| Handshake CPU per side (excluding network RTT and Argon2 unlock) | < 5 ms |
| Added latency for a TEXT frame vs raw TCP | < 1 ms |
| AEAD + chain throughput, single core, full chunks | ≥ 300 MB/s |
| Image transfer, gigabit LAN, end to end | ≥ 90 MB/s |
| Heap allocations per steady-state frame (send and receive) | ≤ 8 small ones (stdlib `hmac.New`/AEAD construction); **zero** frame-sized allocations — those come from `internal/buf` pools |
| CLI RSS, idle, one peer (after `debug.FreeOSMemory` post-unlock) | < 30 MiB |
| GUI RSS, idle, one peer | < 200 MiB (raised from 150 by the user on 2026-09-29: about 95 MiB is graphics driver memory) |
| CLI startup to ready after passphrase unlock | < 300 ms |
| Argon2id unlock (t=3, 64 MiB, p=4) | ~100–300 ms once per launch; acceptable |
| Idle CPU | < 0.5 % |
| Rekey cost (3 × X25519 + ML-KEM-768 keygen/encaps/decaps) | < 1.5 ms; never stalls the writer for more than one frame |

Implementation guidance: one `bufio.Writer` (256 KiB) per connection; **flush after every
stream-0 and stream-1 frame** and otherwise when the queue drains or the buffer fills (so
chat and control never sit behind buffered chunks); one reusable read buffer per
connection; `sync.Pool` for frame buffers; no `fmt.Sprintf` on the hot path; `-benchmem`
everywhere and `testing.AllocsPerRun` assertions in `session_test.go`. Disk I/O for
transfers happens on the per-transfer file-writer/file-reader goroutines (bounded channels
of 4 chunks in, 2 out), never on the reader or writer.

---

## 12. Stability requirements

- **No goroutine leaks**: every integration test ends with `goleak.VerifyNone`.
- **Timeouts everywhere**: handshake 10 s, read deadline 90 s (refreshed per frame), write
  deadline 30 s per frame, image accept prompt 10 min then auto-reject.
- **Reconnect**: for contacts with a known address, exponential backoff 1 s → 60 s with
  full jitter, unlimited attempts while the app runs; user can cancel; `Reconnecting`
  events. Each attempt is a fresh handshake.
- **Session replacement and glare**: a new fully authenticated session (both HELLOs) from a
  `PeerID` that already has a session **replaces** the old one (close it with `BYE
  reason=3`) — only the key holder can create it, so this is safe and it lets a restarted
  peer back in immediately. Exception: if the old session was established < 5 s ago by the
  local clock (true simultaneous dial), keep the session whose initiator has the
  lexicographically smaller public key. Near the 5 s boundary the two sides may disagree
  and close both; reconnect heals it — this is expected, not a bug. Test both paths.
- **Limits**: at most 32 concurrent peers (config); beyond that, new handshakes are closed
  silently.
- **Message queue**: `SendText` while offline queues in memory (bounded, 1,000 messages;
  beyond that it returns `ErrQueueFull`). Status transitions `pending → sent → delivered`;
  `failed` only on explicit cancel or unrecoverable error. **On session loss every `sent`
  but undelivered message reverts to `pending`**; queued messages are sent after the next
  successful HELLO exchange, in order, with their original `msg_id`s (the peer's dedup makes
  this idempotent).
- **Session state machine** is explicit and documented in `docs/PROTOCOL.md`: connection
  `Handshaking → Hello → Established → Closing → Closed`; rekey sub-state — initiator:
  `rekeyInFlight` (writer-owned), responder: `readerRekeyState ∈ {Idle, AwaitingDone}`
  plus writer-owned `requestSentAt`; per-stream
  `Offered → Accepted → Transferring → Done | Draining → Closed`. Any invalid transition
  closes the session with a logged reason. No panics.
- **Crash safety**: all stores atomic (§7); on startup, partial transfers with valid
  metadata are listed as resumable; `burn` removes them.
- **Graceful shutdown**: SIGINT/SIGTERM → send BYE to each peer whose session is
  Established (best effort, 2 s), close
  listeners, clear secrets, flush stores, release the lock, exit 0.
- **Panic policy**: a panic in a session goroutine is a bug; the supervisor recovers, logs a
  redacted stack, tears down that session, and keeps the process alive. In tests, panics
  fail the test.

---

## 13. Testing

All of these exist and run in CI (`.github/workflows/ci.yml`; matrix linux/macos/windows;
latest stable Go; CI installs Fyne's build dependencies and runs `make test-gui` and
`make build-gui` in addition to `make test`).

1. **Known-answer tests** — Noise `XK_25519_ChaChaPoly_BLAKE2s` against the cacophony
   vectors flynn/noise itself tests with (fixture committed under `testdata/`); HKDF (RFC
   5869) and HMAC (RFC 4231) vectors; ML-KEM encaps/decaps round-trip plus a fixed-seed
   check; Argon2id checked against the `x/crypto/argon2` test vectors (no secret/AD).
2. **Protocol tests** (`internal/session`, `internal/handshake`) — two in-process peers over
   `net.Pipe()`: handshake, HELLO, text both ways, ACK, PING/PONG, at least three rekey
   epochs *while* an image transfer is in flight **in both directions**, REKEY_REQUEST path,
   controller `StartRekey` arriving between RESP and DONE (must be discarded), BYE,
   handshake failure reporting.
3. **Adversarial tests** using `internal/testpeer` (a scriptable peer that can misbehave):
   wrong responder key (initiator must abort), unknown initiator without token (responder
   silent + close), expired/used/reserved token, multi-use invite over 64 contacts,
   **replayed msg1 (rejected before DH)**, handshake message with wrong length, invalid
   `kem_ek` encoding, truncated/oversized/undersized outer length (rejected before reading
   the body), flipped ciphertext bit (session closes, no BYE), non-conforming padding size,
   non-zero `flags`, frame before HELLO, second HELLO, HELLO with reserved feature bits or
   inconsistent `max_image`, every illegal row of the §3.5 control-frame table, PONG with
   wrong nonce, PING flood, **a peer holding session state but not the identity key cannot
   complete a rekey** (next epoch fails on both sides), duplicate `msg_id`, ACK for unknown
   `msg_id` (ignored), invalid UTF-8, control characters, bidi overrides, zero-width
   characters, combining-mark flood, ZWJ runs, image `format` lying about magic bytes,
   decompression-bomb dimensions, GIF with 201 frames, offer over `max_image`, file offer
   over `max_file`, file offer with an illegal extension byte, file offer to a peer that
   did not advertise files, 5th pending
   offer, offer churn (17 in 60 s), wrong-parity stream id, reused stream id, chunk index
   out of order, wrong chunk size, wrong final hash, unknown IMG_CANCEL reason, cancel races
   (both sides cancel; cancel after DONE), 17 draining streams, a 65,537th frame in one
   epoch (counter 65,536 — refused synchronously by the writer on send and the reader on
   receipt), duplicate PONG, REKEY_REQUEST unanswered for 10 s, session replacement and
   simultaneous-dial glare.
4. **Fuzzing** (native Go fuzzing) — `FuzzOuterFrame`, `FuzzInner`, one `Fuzz<Type>Decode`
   per frame payload, `FuzzHandshakePayloads`, `FuzzInviteParse`, `FuzzJPEGStrip`,
   `FuzzPNGStrip`, `FuzzWebPStrip`, `FuzzGIFStrip`, `FuzzGIFPrescan`, `FuzzSanitize`,
   `FuzzNormalize`, `FuzzStoreBlob`, `FuzzMasterHeader`. Seed corpora committed under
   `testdata/fuzz`. Nightly CI runs each for 10 minutes.
5. **Media tests** — fixtures generated in-test (not downloaded) with EXIF/GPS/ICC/XMP/
   comments/trailing bytes in every format; assert the stripped output contains none of
   them, is byte-identical across two stripping passes, and decodes to identical pixels on
   lossless paths (near-identical on the rotation path); animated WebP rejected; oriented
   WebP comes out as PNG.
6. **Store tests** — round-trip, wrong passphrase fails cleanly, tampered blob fails,
   relpath swap fails (AD), header out-of-bounds parameters refused, mode 1 ↔ mode 2
   conversion, second-process lock refused, crash injected between **every** step of the
   directory swap (including the both-present/no-`store` state) → old or new store intact
   with the right passphrase, never a mix, never "no identity".
7. **Transport conformance** — every `Transport` implementation passes
   `internal/transport/conformance`.
8. **End-to-end** — spawn two `burrow --plain --json` processes on loopback, exchange text
   and an image, kill one mid-transfer, restart, verify queued message delivery (including
   `sent`→`pending` reversion) and image resume via `.meta` (with truncate + re-hash).
9. **Race + leak** — everything under `-race`; `goleak` in all integration tests.
10. **Benchmarks** — §11 budgets encoded as benchmarks; `make bench` compares against
    `bench/baseline.json` and fails on > 15 % regression.
11. **Static** — `go vet`, `staticcheck`, `gosec`, `govulncheck`, `golangci-lint` with
    `depguard` enforcing §2.2 and §14.
12. **Coverage** — ≥ 90 % in `internal/wire`, `internal/session`, `internal/handshake`,
    `internal/media`, `internal/invite`, `internal/text`; ≥ 80 % elsewhere under `internal/`.

---

## 14. Dependency allowlist

Standard library first. Beyond it, only:

| Module | Used by | Why |
|---|---|---|
| `golang.org/x/crypto` | identity, invite, handshake, session, store, media | argon2, blake2b, chacha20poly1305, curve25519 |
| `golang.org/x/image` | media | WebP decoding |
| `golang.org/x/text` | text | NFKC normalization, case folding |
| `golang.org/x/term` | cmd/burrow | no-echo passphrase and invite prompts |
| `golang.org/x/sys` | store, transport, cmd/* | Windows ACL checks, socket options, core-dump limits |
| `golang.org/x/net` (`dns/dnsmessage` only) | transport/mdns (Phase 4) | mDNS packet building |
| `github.com/flynn/noise` | handshake | Noise XK (small, mature) |
| Bubble Tea + lipgloss + bubbles (`charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2` — the v2 modules moved off github.com; verified with `go get` in Phase 1) | ui/tui | TUI |
| `fyne.io/fyne/v2` | ui/gui (tag `gui`) | GUI |
| `github.com/skip2/go-qrcode` | ui/gui, ui/tui, cmd/burrow (`id --qr`, `invite --qr` text QR) | invite QR — small and stable; accepted as frozen |
| `github.com/BurntSushi/toml` | store | config |
| `github.com/cretz/bine` | transport/tor (Phase 4) | Tor control — small; accepted as frozen, vendor if it disappears |
| `go.uber.org/goleak` | tests only | leak detection |
| `github.com/awnumar/memguard` | Phase 5, build-tagged, ask first | mlock for identity key |

Pin versions in `go.mod`, commit `go.sum`, run `govulncheck` in CI, and check each
module's maintenance status before adding it (a dead dependency is a liability; the two
marked "frozen" are the accepted exceptions). If `flynn/noise` proves unsuitable,
`github.com/katzenpost/nyquist` is the alternative to evaluate — ask first. No other
module without asking.

---

## 15. Threat model (keep `docs/THREAT_MODEL.md` in sync)

**We protect against**

- Passive network observers, now and with future quantum computers (hybrid root key), for
  **message and image content**.
- Active MITM (pinned static keys, XK mutual auth, safety-number verification, hard failure
  on key mismatch, no key-change prompts).
- Compromise of a long-term key *after* a conversation (per-session forward secrecy).
- Compromise of the running session's state: per-message forward secrecy within an epoch,
  and eviction at the next rekey of any attacker who holds session state but not a
  long-term private key — even an active one (static-key-bound rekey, §3.5).
- Enumeration by scanners that do not hold a peer's public key (silent responder; key
  never transmitted; msg1 replay rejected; invite tokens).
- Leakage through image metadata (stripping; no filenames; no timestamps or peer ids in
  saved or partial file names).
- Theft of the disk (encrypted identity/contacts/invites/transfer metadata; no history by
  default; no core dumps).
- Terminal/UI injection and name spoofing via crafted text (sanitization, normalization).
- Memory exhaustion and decompression bombs from a malicious peer (hard caps before
  allocation; bounded queues; per-peer stream, offer and churn caps; decode semaphore).
- A second local process corrupting the store (exclusive lock).

**We do not protect against (say so in the README)**

- A compromised endpoint (malware, keyloggers, screen capture), including an attacker who
  holds **both** a long-term private key and session state — they persist until the
  identity is rotated.
- A malicious peer sharing what they legitimately received.
- **Anyone who holds a peer's public key** — every past invite recipient, including blocked
  contacts — can detect whether that peer is online at a given address (they get msg2
  instead of silence). A recording of an earlier msg1 does the same until the responder
  process restarts. Rotating identity is the only remedy.
- **Protocol fingerprinting**: the fixed handshake sizes (1232/1168/113) and the 256/4096
  frame buckets identify Burrow to any DPI box, and a scanner learns that a Burrow
  listener is on a port (accept, then silence after ~1232 bytes).
- **Traffic analysis by any single network observer**: message timing, coarse sizes, rekey
  events (the 1296/1296/272-byte ciphertext pattern), TYPING cadence when enabled, and
  reconnect attempts (presence beacons to the last known address). Padding reduces; Tor
  helps much more; neither eliminates it.
- **Quantum de-anonymization of recorded handshakes**: the initiator's identity and invite
  token in msg3 are protected only by X25519. A future quantum attacker with a recording
  learns *who* talked to whom, but not *what* (content is hybrid-protected).
- Quantum attacks on *authentication* (a future quantum attacker could impersonate in new
  sessions but cannot decrypt recorded ones). Revisit with ML-DSA in protocol v2.
- IP exposure to the peer in direct-TCP mode (use the Tor transport to hide it).
- Encoder fingerprints inside images in `strip` mode (quantization tables, zlib settings
  can identify camera or software models); `paranoid` mode removes them.
- **Metadata inside files that are not images** (§6.5): author and software fields in
  documents, names and timestamps of the files inside an archive. They are sent as they
  are. The file's own name and timestamps are never sent; its extension is.
- What a received file does when the user opens it. Burrow never opens it.
- Store rollback: restoring an older `invites` blob from a backup resurrects a consumed
  single-use token (endpoint compromise territory).
- Perfect memory zeroization under Go's GC or inside library objects; secrets paged to swap
  without memguard.
- Denial of service beyond rate limiting and hard caps (a key holder can occupy handshake
  slots).

---

## 16. Roadmap and definition of done per phase

Build phases in order. Finish and document each before starting the next.

**Phase 0 — Skeleton.** `docs/STATUS.md` first, then module, layout, Makefile,
`.golangci.yml`, CI matrix, `internal/wire`
with every constant and every encode/decode + fuzz targets, `internal/secret`,
`internal/buf`, `internal/text`, `internal/identity` (keys, fingerprint, safety number),
`internal/invite`, `internal/store` (master header both modes, lock, atomic encrypted
blobs, directory-swap passphrase change with full recovery). *DoD:* §13 items 1, 4
(wire/invite/store/text), 6, 11 pass.

**Phase 1 — Secure text over direct TCP (CLI only).** Handshake (with Authorize callback
and replay LRU), hybrid root, chains, rekey state machine with static-key binding,
multiplexer, HELLO/PING/PONG/TEXT/ACK/TYPING/BYE, invites, contacts, handshake-failure +
name-collision handling, reconnect + queue + replacement/glare, `--plain`/`--json`, TUI
without images. *DoD:* §13 items 2, 3 (all non-image cases), 4 (`FuzzHandshakePayloads`),
7, 8 (text part), 9, 10 (handshake + frame benchmarks), 12.

**Phase 2 — Images.** `internal/media` gates, stripping, two-pass send on the file-reader
goroutine, IMG_* frames and stream state machine, file-writer goroutine, resume with
`.meta`, terminal image rendering (Kitty, iTerm2). *DoD:* §13 items 3 (image cases), 4
(media fuzzers), 5, 8 (image part), and the image throughput budget.

**Phase 3 — GUI.** Fyne app behind the `gui` tag with every flow in §10; identical defaults;
QR invites. *DoD:* manual test script in `docs/gui-test-plan.md` executed on all three
OSes; no engine changes were needed (if they were, they have tests).

**Phase 4 — Privacy transports and options.** Tor onion transport (key in identity blob),
opt-in mDNS with hashed announcements, optional encrypted history + `burn`,
`auto_accept_from_verified`. *DoD:* transport conformance for tor; history round-trip
tests; README documents the trade-offs.

**Phase 5 — Hardening and release.** Extended fuzz corpora, memguard option, reproducible
release builds with checksums, `docs/SECURITY.md` with a disclosure process, the checklist
in §17, and a `PROTOCOL.md` complete enough for an independent implementation.

---

## 17. Pre-release security checklist

- [ ] Every constant in §3–§4 matches `internal/wire` and `docs/PROTOCOL.md` (`make docs-check`).
- [ ] `govulncheck` clean; all dependencies at latest tagged versions and still maintained
      (frozen exceptions reviewed).
- [ ] All fuzzers have run ≥ 1 hour each with no findings.
- [ ] Adversarial suite (§13.3) green on all three OSes.
- [ ] No log line can contain content: grep every `slog`/`log` call site and confirm
      `text.Redact()` on peer strings and invites.
- [ ] Defaults audit: typing off, timestamps on (seconds), auto-accept off, mDNS off,
      history off, paranoid images off (strip on), open-links off, display name empty,
      passphrase required, invite TTL 1 h single-use, largest image 25 MiB, largest file
      100 MiB.
- [ ] `strings`/`grep` the release binary for stray debug output, paths, or usernames
      (`-trimpath` set).
- [ ] Reproducible build verified from a clean clone on two machines.
- [ ] README states exactly what is and is not protected (§15).

---

## 18. Decisions log (why, and what was rejected)

| Decision | Rationale | Rejected |
|---|---|---|
| Noise **XK** | Responder key never on the wire; silent to key-less scanners; initiator identity sent under forward-secret keys; 1.5 RTT is negligible for a long-lived chat connection | **XX** (responder identity revealed to any active prober), **IK** (initiator identity not forward-secret; the 1-RTT gain is irrelevant here) |
| Hybrid PQ via KEM-in-payload + random exports → HKDF root | Uses standard Noise unchanged; the PQ secret provably enters the root; no dependence on a library's PQ pattern support | Noise PSK tricks (PSK must be known at handshake start), PQ-Noise libraries (maturity risk) |
| Symmetric chains + periodic hybrid rekey instead of a full Double Ratchet | The transport is ordered and reliable, so skipped-key storage and out-of-order handling are dead weight; same FS/PCS properties | Signal Double Ratchet (designed for asynchronous, unordered delivery) |
| Rekey mixes `es`/`se` DHs against the long-term keys | Three extra X25519 per rekey buy eviction of an *active* attacker who stole session state but not the identity key; without them PCS held only against passive attackers | Ephemeral-only rekey |
| Deterministic epoch switch via REKEY_DONE + encrypt-at-write | No cleartext epoch field; both sides know exactly which frame is the last of an epoch; no trial decryption | Cleartext epoch byte, trial decryption (ambiguous, slow) |
| Reader owns root and derivations; writer owns the in-flight flag and generates the initiator's ephemerals | Every rekey message arrives through the reader, so no cross-goroutine synchronous hand-off is needed; the writer is the only place a rekey can start, so dedup is trivial | A controller goroutine serving chain requests synchronously (deadlock-prone under backpressure) |
| Keys never change; no TOFU key rotation | A "key changed" prompt trains users to click through; a new key is a new person until proven otherwise | TOFU with accept-new-key prompts |
| Hand-written wire codec | Smallest attack surface, byte-exact, easy to fuzz, no reflection | Protobuf/CBOR (dependency + reflection surface on the untrusted path) |
| ChaCha20-Poly1305 everywhere | Constant time in pure Go on every CPU; fast; one suite, no agility | AES-GCM (timing risk without AES-NI), negotiation |
| Random 64-bit `msg_id` | Idempotent resends without leaking message counts | Sequential ids |
| Protocol violations close without BYE | Never tell an attacker which probe was detected | BYE with a reason for every close |
| Direct TCP first, Tor second | Fastest path to a working, testable core; Tor is the real privacy transport and layers cleanly | libp2p, public STUN/TURN |
| mDNS announces a keyed hash, never probes | A contact recognizes you with one hash; strangers learn nothing; no failed-handshake storms | Probing every LAN instance with every contact key |
| Fyne behind a build tag | Pure Go except OpenGL; image canvas built in; the tag keeps the engine buildable and testable without graphics libraries | Wails (webview + JS surface), Gio (more work) |
| Encrypted flat files, one Argon2 run per process, exclusive lock, directory-swap rekey | No SQLite (CGo or a large pure-Go port); tiny surface; atomic writes trivial; unlock cost paid once; no two-writer corruption | SQLite, BoltDB, per-file salts, multi-file in-place rewrites |
| No message history by default | Nothing to seize; users opt in knowingly | Persistent history by default |
| Invite tokens, consumed only after key confirmation | Solves "responder must accept a stranger" without becoming enumerable; a dead handshake burns nothing | Open acceptance of any initiator; consuming at msg3 |
| Files travel with an extension, never a name; images given as files are still stripped | The name is metadata the receiver does not need; the type is what makes the file usable. Stripping stays the default for anything that is a picture | Sending the file name; sending photos untouched through the file path |
| Separate `max_file` in HELLO, 0 = refuse | The sender learns the limit before hashing gigabytes, and a user who wants no files is never even asked | One shared limit; rejecting only after the offer |
| Two-pass deterministic stripping | Hash for the offer without buffering the file or touching disk | Temp files (secrets on disk), whole-file buffering |

---

*End of CLAUDE.md. If you have read this far: run `make test` before you claim anything works.*
