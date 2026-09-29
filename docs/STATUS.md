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
Code review, round 1 of several (five independent reviewers, about 60 findings).
- FIXED with tests: internal/media, text, identity, invite, handshake, session,
  core, store (locking, swap, recovery, blob names).
- NOT STARTED: internal/transport/tor (tor.go `start` kills its own tor process: the
  bootstrap context is passed to tor.Start; data dir lands in the working directory;
  tor output goes to stdout; onion key never wiped), mdns rate limit in `Browse`,
  `store.LoadConfig` validation, scripts/cover.sh, bench/cmp, `make fuzz`, go mod tidy.
- NOT STARTED: every user-interface finding (contact names with spaces in
  `/msg` and `/rename`, forgeable verified mark in `Names.Label`, multi-line text forging
  lines, data race on `Controller.Current`, unbounded scrollback, flags after the
  target ignored, GUI shutdown on Quit, blocking calls on the Fyne thread, paths with
  spaces in `/file`).
- Docs not yet updated for this round: CLAUDE.md §7 (lock path), PROTOCOL.md,
  SECURITY.md, THREAT_MODEL.md, PHASE-5.md.
- `make lint`, `make test` (full), `make test-gui`, coverage and bench not run yet.

## Next step
- Finish the list above, run the full gate, then start review round 2 from scratch.
  After a clean round: the readability and comments pass the user asked for.

## Open questions for the user
- The store lock moved from `<data>/store/lock` to `<data>/store.lock` (the old path
  cannot be held across a passphrase change, and not at all on Windows). CLAUDE.md §7
  still names the old path. Confirm, or say which way to go.
- File transfer choices made without asking (docs/phases/PHASE-5.md): no file name on
  the wire (extension only), v1 amended instead of v2, default limit 100 MiB, files
  saved in the image folder, auto-accept never for files.
- Earlier unconfirmed choices: §0.3 placeholders (module path, port 47337, Bubble Tea v2),
  reconnect policy, `/view` for inline images.
