# STATUS — read this first

## Current phase
Phase 1 — Secure text over direct TCP (CLAUDE.md §16).

## Done
- Phase 0 in full (see docs/phases/PHASE-0.md).
- `internal/handshake`: Noise XK + ML-KEM bootstrap, Authorize callback, msg1 replay LRU, failure
  limiter, fixed lengths, deadlines; tests + benchmark (~0.9 ms round trip).
- `internal/session`: chains, frame AEAD (8 allocs/frame, ~1.1 GB/s seal+open), rekey state machine
  with static-key binding, writer/reader/controller, supervisor; honest + adversarial suites (§13.2,
  §13.3 non-image rows) green under -race with goleak.
- `internal/testpeer`: independent chain/rekey implementation (interop-checked).
- `internal/transport` (+ tcp, conformance suite). `store.Config` (TOML, defaults audit test).
- `internal/core`: engine with invites/contacts/queue/dedup/reconnect/replacement/glare/rate limit/
  graceful BYE; integration tests green (81% cov).
- `internal/ui/common`: event formatting (text + JSON) and the shared slash-command controller.

## Mid-flight
- `cmd/burrow/main_wip.go` + `unlock.go` (build tag `cli_wip`): flag parsing, dispatch and the no-echo
  prompt are written; the subcommand functions they call (`cmdInit`, `cmdID`, `cmdInvite`, `cmdRun`,
  `cmdContacts`, `cmdConfig`, `cmdPassphrase`, `cmdBurn`) do not exist yet, so the tag keeps the
  default build green. `harden_*.go` and `tty_*.go` are final.

## Next step
1. Write `cmd/burrow/cmds.go` (init/id/invite/contacts/config/passphrase/burn), `run.go` (engine
   setup for listen/connect), `plain.go` (stdin commands → `common.Controller`, stdout events via
   `Names.Line`/`Names.JSON`), then drop the `cli_wip` tag.
2. `internal/ui/tui` on `charm.land/bubbletea/v2` v2.0.10 (View() returns `tea.View`, `tea.NewView`,
   `KeyPressMsg.String()`, `Program.Send` for engine events; bubbles v2 textinput/viewport).
3. End-to-end test (§13.8 text part) with two `burrow --plain --json --insecure-no-passphrase` processes.
4. `bench/cmp` + `bench/baseline.json`; PROTOCOL.md state-machine section; PHASE-1.md; lint/test gate.

## Open questions for the user
- Per-frame allocation budget (§11) is exactly 8 per direction, which is the floor for stdlib
  `hmac.New` (5) + two `Sum` (2) + AEAD construction (1); asserted without -race (`race_test.go`).
- flynn/noise ignores `Config.EphemeralKeypair` for non-pre-message patterns; we let it generate the
  handshake ephemeral and wipe it via `LocalEphemeral()` afterwards (note for SECURITY.md).
- Reconnect policy chosen: only after a lost connection or BYE reason 2/4 (never after a deliberate
  BYE, our own close, or a protocol violation) to avoid presence beacons.
- GitHub repo rename to lowercase could not be done here (no `gh`/token); the local remote already
  points at `git@github.com:guy5116/burrow.git`, which works either way.
