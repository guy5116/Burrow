# Phase 3 — GUI

## Built

- `internal/ui/gui` (build tag `gui`) on Fyne v2.8.1 and `cmd/burrow-gui`:
  - Unlock prompt, identity wizard (passphrase twice or the loud insecure option), and a
    "Store in use" dialog when the CLI holds the lock.
  - Main window: contact sidebar (● online, ✓ verified), plain-text conversation with
    per-message delivery marks, multi-line composer (Enter sends), status bar with the
    selected contact's short fingerprint and verification state (never a green "secure"
    marker for unverified peers).
  - Dialogs: invite (host, TTL, multi-use; QR via go-qrcode + copyable string), connect
    (password-style paste field), verify (12 groups in large monospace + both
    fingerprints, "Mark as verified"), new-peer / name-collision / handshake-failed
    information dialogs with fingerprints, settings (every §10.1 toggle, saved to the
    shared `config.toml`), passphrase change, image offer (Reject is the highlighted
    default; nothing is downloaded before Accept).
  - Images: file chooser and window drag-and-drop offer a file; thumbnails come from
    `core.DecodeImage` on a worker goroutine; a viewer window toggles fit / 1:1.
  - Links are shown as text; "Copy link" always, "Open in browser" only behind
    `open_links` and a confirmation showing the full URL.
  - Every engine event is drained on a dedicated goroutine and marshalled with `fyne.Do`.
- Tests (`make test-gui`, Fyne's headless test driver): link detection and rendering
  rules, event → conversation/status updates, send validation, offer dialog lifecycle,
  wizard and unlock validation.
- `docs/gui-test-plan.md`: the manual script for Linux, macOS and Windows.

## Engine changes

None were needed. The GUI uses only `core` and `ui/common`.

## Unspecified choices

- The GUI shares `common.Controller` with the CLI for contact naming and transfer
  numbering (no command execution).
- Message text renders in a `widget.Label` prefixed with `me:` / `them:`; no bubbles or
  markdown (Fyne's RichText markdown is deliberately unused).
- Settings that affect the engine (port, name, limits, reconnect) apply on restart; the
  dialog says so.
- The offer dialog auto-rejects when dismissed any way other than Accept.

## Known issues

- The manual plan has not been executed on macOS or Windows in this session; CI builds
  the GUI on all three and runs the headless tests.
- Fyne pulls a large transitive tree (go-text, goldmark, oksvg, …); see DEPENDENCIES.md.
