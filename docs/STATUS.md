# STATUS — read this first

## Current phase
Phase 1 — Secure text over direct TCP (CLAUDE.md §16). Phase 0 is complete: see docs/phases/PHASE-0.md.

## Done
- Phase 0 in full (wire, secret, buf, text, identity, invite, store, KATs, docs, CI, lint).

## Mid-flight
- (nothing)

## Next step
- `internal/handshake`: Noise XK + ML-KEM bootstrap → root key, `Authorize` callback, msg1
  replay LRU, fixed message lengths, deadlines. Tests first (§13.2/§13.3 handshake cases),
  then `internal/session` (chains, frame AEAD, rekey state machine, multiplexer).

## Open questions for the user
- §0.3 placeholders adopted without confirmation: module path `github.com/guy5116/burrow`
  (the GitHub remote is spelled `Burrow` — rename the repo to lowercase or change the
  module path), default port 47337, Bubble Tea v2 (`github.com/charmbracelet/bubbletea/v2`).
