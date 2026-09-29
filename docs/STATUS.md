# STATUS — read this first

## Current phase
Phase 5 — Hardening and release: implementation complete, release gate partially open.
See docs/phases/PHASE-5.md and docs/RELEASE_CHECKLIST.md.

## Done
- Phases 0–4 in full; Phase 5 hardening, coverage floors, log audit, reproducible-build
  check on one machine, disclosure process, protocol document, release checklist.
- Spec gap closure (table in docs/phases/PHASE-5.md) and a README for first-time users.
- memguard build tag for the identity key (approved by the user).

## Mid-flight
- (nothing)

## Next step
- Close the open items in docs/RELEASE_CHECKLIST.md: ≥ 1 h fuzzing per target, a green CI
  run on all three OSes, reproducibility on a second machine, Tor conformance with tor
  installed, and the manual GUI plan.

## Open questions for the user
- Earlier unconfirmed choices: §0.3 placeholders (module path, port 47337, Bubble Tea v2),
  engine mutex instead of a single goroutine, reconnect policy, `/view` for inline images.
