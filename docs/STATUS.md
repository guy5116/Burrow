# STATUS — read this first

## Current phase
Phase 5 — Hardening and release (CLAUDE.md §16). Phase 4 is complete: see
docs/phases/PHASE-4.md.

## Done
- Phases 0–4 in full.

## Mid-flight
- (nothing)

## Next step
- Extended fuzz corpora (run every fuzzer ≥ 1 h, commit crashers), memguard option (ask
  first), reproducible release builds verified from a clean clone, `docs/SECURITY.md`
  disclosure process, the §17 checklist, and a `PROTOCOL.md` complete enough for an
  independent implementation.

## Open questions for the user
- Phase 4 choices are in docs/phases/PHASE-4.md. Tor conformance has not been run here
  (no tor binary); run `BURROW_TOR_TEST=1 go test ./internal/transport/tor` with tor installed.
- GitHub repo rename to lowercase still needs doing in the repository settings.
