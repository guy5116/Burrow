# STATUS — read this first

## Current phase
Phase 3 — GUI (CLAUDE.md §16). Phase 2 is complete: see docs/phases/PHASE-2.md.

## Done
- Phases 0, 1 and 2 in full.

## Mid-flight
- (nothing)

## Next step
- `internal/ui/gui` + `cmd/burrow-gui` behind the `gui` tag on Fyne v2 (check `fyne.Do` /
  `fyne.DoAndWait` for the current API): unlock dialog, identity wizard, main window
  (sidebar + conversation + composer), image bubbles from `core.DecodeImage`, verify
  dialog, invite/paste dialogs with QR, settings; `docs/gui-test-plan.md`.

## Open questions for the user
- Phase 2 choices are listed in docs/phases/PHASE-2.md (inline images on demand via /view;
  paranoid animated GIF stays GIF).
- GitHub repo rename to lowercase still needs doing in the repository settings.
