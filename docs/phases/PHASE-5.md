# Phase 5 — Hardening and release

## Built

- Coverage raised to the §13.12 floors with targeted tests: truncation of every image
  fixture at every offset, write-failure propagation, structural edge cases in all four
  strippers; session config/not-ready/controller/supervisor/pending/stream-state paths;
  handshake error and limiter eviction paths; mDNS foreign and malformed records; core
  error mappings, corrupt state, partial cleanup and image edge cases with a raw peer; the
  shared command controller, event formatting, terminal image framing and the TUI model.
  `scripts/cover.sh` enforces the floors and runs in CI.
- Bugs found while hardening, all fixed with tests:
  - live TEXT sends could be reordered (one goroutine per message) → one ordered sender
    per session;
  - a simultaneous IMG_CANCEL could drop one side's CANCEL and leave the peer draining;
  - frames queued behind a closed stream could follow the close → per-stream done
    channel and fresh queues for terminal frames;
  - the offline queue's status was read without the engine lock during dial glare.
- Log hygiene: panic stacks are reduced to function names and file:line; inbound session
  failures log a reason category instead of an error string that could carry an address.
- Reproducible release builds verified from two clean clones at different paths
  (identical SHA-256 for linux/darwin/windows); binary scanned for paths and usernames.
- `docs/SECURITY.md`: private disclosure process with timelines and scope.
- `docs/PROTOCOL.md`: control-frame legality, rekey schedule and fail-safes, complete
  stream rules and limits, text sanitization and normalization, session replacement,
  delivery and dedup, at-rest formats — enough for an independent implementation.
- `docs/RELEASE_CHECKLIST.md`: the §17 checklist with evidence and what remains open.
- `make fuzz` now includes `internal/transport/mdns`.

## Not done (needs the user or other machines)

- `memguard` build tag: CLAUDE.md requires asking first; not added.
- Fuzzing for ≥ 1 hour per target; a second machine for the reproducibility check; CI
  observation on macOS and Windows; Tor conformance with a tor binary; the manual GUI plan.

## Unspecified choices

- `scripts/cover.sh` is a script rather than a Makefile target because §1.1 fixes the
  Makefile's target list.
- Coverage exemptions are limited to test-support packages and the Tor transport.
