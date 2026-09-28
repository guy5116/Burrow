# STATUS — read this first

## Current phase
Phase 2 — Images (CLAUDE.md §16). Phase 1 is complete: see docs/phases/PHASE-1.md.

## Done
- Phases 0 and 1 in full.

## Mid-flight
- (nothing)

## Next step
- `internal/media`: size gates (`image.DecodeConfig`, GIF pre-scan), magic sniffing, metadata
  stripping for JPEG/PNG/WebP/GIF (strip mode) with EXIF orientation, paranoid mode, two-pass
  deterministic hashing; tests with in-test fixtures and the media fuzzers. Then the IMG_*
  stream state machine in `internal/session` (per-transfer queues, file-writer/file-reader
  goroutines), resume via `.meta`, core API (`SendImage`/`AcceptImage`/…), CLI `/image`,
  `/accept`, `/reject`, Kitty/iTerm2 rendering, end-to-end image part.

## Open questions for the user
- Decisions made without confirmation are listed in docs/phases/PHASE-1.md (engine mutex instead
  of a single goroutine; reconnect policy; 8-alloc budget at the stdlib floor).
- GitHub repo rename to lowercase still needs doing in the repository settings.
