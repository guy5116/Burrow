# STATUS — read this first

## Current phase
Phase 4 — Privacy transports and options (CLAUDE.md §16). Phase 3 is complete: see
docs/phases/PHASE-3.md.

## Done
- Phases 0–3 in full.

## Mid-flight
- (nothing)

## Next step
- Tor onion transport (`internal/transport/tor` via `github.com/cretz/bine`; onion key in
  the identity blob → bump the identity blob version), then opt-in mDNS discovery
  (`internal/transport/mdns` with `identity.MDNSTag`), optional encrypted history + `burn`,
  README trade-offs. Each transport must pass `internal/transport/conformance`.

## Open questions for the user
- Phase 3 choices are in docs/phases/PHASE-3.md; the manual GUI plan needs a run on
  macOS and Windows.
- GitHub repo rename to lowercase still needs doing in the repository settings.
