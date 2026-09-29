# STATUS — read this first

## Current phase
Phase 5 — Hardening and release: implementation complete, release gate partially open.
See docs/phases/PHASE-5.md and docs/RELEASE_CHECKLIST.md.

## Done
- Phases 0–4 in full; Phase 5 hardening, coverage floors, log audit, reproducible-build
  check on one machine, disclosure process, protocol document, release checklist.
- Spec gap closure and second audit (tables in docs/phases/PHASE-5.md), README for
  first-time users, memguard build tag (approved by the user).
- Engine is one goroutine (§2.3); sessions have exactly three; chunks go reader →
  file-writer; typing notices are the only events dropped for a slow screen.
- Go 1.26 minimum (user decision, recorded in CLAUDE.md §1.2).

## Mid-flight
- (nothing)

## Next step
- Close the open items in docs/RELEASE_CHECKLIST.md: ≥ 1 h fuzzing per target, a green CI
  run on all three OSes, reproducibility on a second machine, Tor conformance with tor
  installed, the manual GUI plan, a look at Kitty inline thumbnails.

## Open questions for the user
- Desktop app idle memory is 172–179 MiB on the development machine, over the 150 MiB
  budget; about 95 MiB is graphics driver memory. Keep the budget, raise it, or measure
  something else (for example memory excluding shared libraries)?
- Earlier unconfirmed choices: §0.3 placeholders (module path, port 47337, Bubble Tea v2),
  reconnect policy, `/view` for inline images.
