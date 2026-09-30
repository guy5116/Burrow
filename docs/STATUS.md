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
- Go 1.26 minimum and a 200 MiB desktop app memory budget (user decisions, recorded in
  CLAUDE.md §1.2 and §11).
- IP visibility warning in the README, `burrow init` and the desktop wizard
  (`common.IPWarning`).
- File transfer for any file type (CLAUDE.md §6.5): `/file`, `max_file_mib`, size shown
  before accepting. Protocol v1 amended in place (HELLO `max_file`, FILE_OFFER 0x27).

## Mid-flight
- Review rounds 1 and 2 are fixed in every package (tables in docs/phases/PHASE-5.md).

## Next step
- Run review round 3 from scratch. Repeat until a round finds nothing that matters,
  then do the pass for layout and comments.
- After that, the open items in docs/RELEASE_CHECKLIST.md: ≥ 1 h fuzzing per target, a
  green CI run on all three OSes, reproducibility on a second machine, Tor with a real
  tor, the manual GUI plan, a look at Kitty inline thumbnails.

## Open questions for the user
- The store lock moved from `<data>/store/lock` to `<data>/store.lock` (the old path
  cannot be held across a passphrase change, and not at all on Windows). CLAUDE.md §7
  now names the new path. Confirm, or say which way to go.
- mDNS announcements can be replayed on another network to find the announcer's
  contacts. Binding the tag to the source address and a time window fixes it but
  changes CLAUDE.md §5. Approve, or keep it documented as a limit (mDNS is off by
  default).
- File transfer choices made without asking (docs/phases/PHASE-5.md): no file name on
  the wire (extension only), v1 amended instead of v2, default limit 100 MiB, files
  saved in the image folder, auto-accept never for files.
- Earlier unconfirmed choices: §0.3 placeholders (module path, port 47337, Bubble Tea v2),
  reconnect policy, `/view` for inline images.
