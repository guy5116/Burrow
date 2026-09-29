# Phase 0 — Skeleton

## Built

- Module `github.com/guy5116/burrow` (go 1.24, toolchain go1.27.1), Makefile with every
  target from CLAUDE.md §1.1, `.golangci.yml` (depguard enforces §2.2 and the §14
  allowlist; verified to catch `x/crypto` in `text` and `unsafe` in `wire`), CI matrix
  (linux/macos/windows, lint job, nightly 10-minute fuzz).
- `internal/secret`: wipeable buffers. `internal/buf`: pooled frame buffers, wiped on Put.
- `internal/wire`: every protocol constant, outer/inner frame codec with the padding rule
  and type/stream pairing enforced, byte-exact encode/decode for all 18 frame types and
  the three handshake payloads, 17 fuzz targets + `FuzzHandshakePayloads`, 100% coverage,
  `TestProtocolDocConstants` (= `make docs-check`) type-checks the package and compares
  every exported constant with the table in `docs/PROTOCOL.md` (`-update` regenerates it).
- `internal/text`: `Sanitize` (three profiles), `Normalize`, `Redact` (HMAC with a
  per-process random key, so log hashes cannot be dictionary-reversed), fuzzers.
- `internal/identity`: keys (RFC 7748 KAT), canonical fingerprints, safety numbers (pinned
  answer), mDNS tag.
- `internal/invite`: strict encode/parse with constant-time checksum compare, hostname/IP/
  onion validation, fuzzer.
- `internal/store`: master header (both modes, bounds), path-bound XChaCha blobs, atomic
  writes with directory fsync, exclusive lock (flock / Windows exclusive open), identity
  blob, directory-swap passphrase/mode change, startup recovery, crash injection at every
  swap step, Argon2id KATs, fuzzers. 82% coverage.
- `internal/handshake`: KATs only (Noise XK cacophony vectors extracted from flynn/noise,
  HKDF RFC 5869, HMAC RFC 4231, ML-KEM-768 round trip + pinned seed digest, handshake
  message sizes 1232/1168/113 confirmed against the real library).
- Docs: PROTOCOL.md, THREAT_MODEL.md, SECURITY.md, DEPENDENCIES.md, README.md.
- `cmd/burrow` / `cmd/burrow-gui` stubs so `make build`, `make cross` and `make build-gui`
  work from Phase 0 on.

## Unspecified choices made (conservative option each time)

- §0.3 placeholders were adopted without confirmation (autonomous session): module path
  `github.com/guy5116/burrow`, port 47337, Bubble Tea **v2** (`charm.land/bubbletea/v2`,
  v2.0.10 at the time; lipgloss/bubbles likewise v2). The GitHub repository was renamed to lowercase `burrow` on 2026-09-29, so the remote now matches the module path.
- `buf.Put` wipes the whole buffer (privacy over the ~2 µs memset per frame).
- Wire-level decode rejects: `ACK.status ≠ 1`, `IMG_OFFER.size = 0` or unknown format,
  `HS3` with `has_token = 0` but non-zero token bytes, empty `IMG_CHUNK` data.
- `Sanitize` drops joiners beyond a run of 2 and combining marks beyond a run of 4 (rather
  than replacing them); a mark run with no base character counts as a run.
- `Redact` output is `[len=N h=xxxxxxxx]`.
- Fingerprint parsing accepts spaces/dashes and either case but requires canonical
  trailing bits.
- Invite `Parse` requires lowercase input, `port ≥ 1`, `expiry > 0`, a non-zero key, RFC
  1123 hostnames, and exactly `56 + ".onion"` for kind 2. Expiry is checked by the caller
  via `Expired(now)` so the parser stays pure.
- Store blob relpaths are limited to `[A-Za-z0-9._-]` segments, ≤ 128 bytes, no `lock`
  or `master.*` names. Blobs are capped at 16 MiB on read.
- `Init` builds the store in `store.init/` and renames it into place; `Recover` deletes a
  stray `store.init/`. A lone `store.new/` (unreachable from the swap) is deleted.
- If deleting `store.old/` fails after a durable passphrase change, `ChangePassphrase`
  returns `ErrOldStoreRemains` (the change is complete; `Recover` cleans up next start).
- Identity blob plaintext is `version u8 = 1 || scalar[32]`; Phase 4 bumps it for the
  onion key.
- Config (TOML) is deferred to Phase 1 with the CLI; `BurntSushi/toml` is therefore not
  yet in `go.mod`.

## Deferred

- `make bench` references `bench/cmp` and `bench/baseline.json`, which arrive with the
  first benchmarks in Phase 1.
- `make test-gui` runs against an empty `internal/ui/gui` until Phase 3.

## Known issues

- Windows lock and paths are compiled (`GOOS=windows go vet`) but only exercised in CI.
- `DefaultPaths` branches for macOS/Windows are not covered on Linux.
