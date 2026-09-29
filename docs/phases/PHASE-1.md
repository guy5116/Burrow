# Phase 1 — Secure text over direct TCP

## Built

- `internal/handshake`: Noise XK + ML-KEM-768 bootstrap → `root_0`; `Authorize(peer, token)`
  callback; msg1 replay LRU (4096 entries, checked before any DH); per-key failure limiter;
  the three fixed message lengths; 10 s / 5 s deadlines; `handshake.Error{Stage}` for events.
- `internal/session`: HMAC chains; ChaCha20-Poly1305 frames with the §3.5 nonce/AD; the
  65,536-frame cap enforced synchronously both ways; rekey state machine exactly as §3.5
  (writer-owned `rekeyInFlight`/`pending`, reader-owned derivation and `readerRekeyState`);
  controller on a 1 s tick with the §3.5 schedule and fail-safes; PING/PONG nonce rules and
  flood limit; TYPING drop limit; strict-priority writer; supervisor with panic recovery;
  every secret cleared at teardown. Honest suite + adversarial suite (every non-image §13.3
  row) under `-race` with `goleak`.
- `internal/testpeer`: independent implementation of chains, framing and both rekey roles,
  used for adversarial and interop tests (including "session state without the identity key
  cannot rekey").
- `internal/transport` interface with `RateKey`, `tcp` transport (keepalives, dual-stack),
  `conformance` suite.
- `internal/core`: engine (a mutex at first, one engine goroutine since the Phase 5 fixes); invites (reserve at msg3, consume after
  both HELLOs, multi-use caps); contacts with name-collision check; message queue with
  dedup, ACK re-send, sent→pending reversion and in-order resend with original ids; session
  replacement and simultaneous-dial glare; per-transport failure rate limiting; graceful
  BYE on shutdown; auto-reconnect with full-jitter backoff.
- `store.Config` (TOML) with the §17 defaults; `burrow config get|set`.
- `cmd/burrow`: every §9.1 subcommand; `--plain`/`--json`; no-echo prompts via `x/term` on
  the controlling tty; core-dump hardening; QR output.
- `internal/ui/common`: event → text/JSON, shared slash-command controller.
- `internal/ui/tui`: Bubble Tea v2 (contacts sidebar, conversation, status bar, input).
- End-to-end test: two `burrow --plain --json` processes, text both ways, kill/restart,
  queued delivery in order after reconnect, persisted contacts, matching safety numbers.
- `bench/cmp` + `bench/baseline.json` (`make bench`).

## Unspecified choices / deviations (conservative option each time)

- §2.3 describes "one engine goroutine"; Phase 1 used one mutex over the bookkeeping
  instead. **Replaced in Phase 5** by the single engine goroutine the spec describes (see
  docs/phases/PHASE-5.md).
- Reconnect only after a lost connection or BYE reason 2/4; never after a deliberate BYE,
  local close, block/remove, or a protocol violation (limits presence beacons).
- Per-frame allocation budget is 8 per direction, the stdlib floor (`hmac.New` 5, two `Sum`
  2, AEAD construction 1); asserted without `-race` (`make test` runs both modes).
- `flynn/noise` generates the handshake ephemeral itself; we wipe it via `LocalEphemeral()`
  (SECURITY.md). Handshake returns only `root_0`; `session.deriveChains` derives epoch 0.
- `Send` on a session blocks under backpressure with a context; `core.SendText` returns
  immediately (since Phase 5 the engine goroutine queues the frame without blocking).
- HELLO exchange timeout reuses `HandshakeTimeout` (10 s).
- Nickname rename: 1–64 characters after Name-profile sanitization.
- Invite host: `--host`, then `invite_host` config, then the first non-loopback IPv4.
- Plain mode: lines without a leading `/` go to the contact selected with `/to` (or the one
  just connected); `Ready`, `Output` and `Error` are extra JSON event kinds for scripting.
- Bubble Tea v2 lives at `charm.land/*/v2`; allowlist and CLAUDE.md §14 updated.
- `burrow contacts safety <contact>` added (not in §9.1) so the number can be compared
  without opening the chat.

## Deferred

- Tor transport (Phase 4): `Connect` on a Tor invite returns `ErrNoTransport`.
- Terminal image rendering, IMG_* frames, per-transfer queues (Phase 2).
- `internal/ui/tui` has no automated tests (Bubble Tea v2 rendering); the controller it
  drives is exercised by the plain-mode end-to-end test.

## Known issues

- The GUI stub (`cmd/burrow-gui`) only builds with `-tags gui`, so `go build ./...` without
  the tag reports "function main is undeclared"; `make build`/`make cross` target
  `./cmd/burrow` only (as specified).
- `TestDeadPeerReadDeadline` and `TestMsg1Deadline` take 90 s and 5 s (skipped with `-short`).
