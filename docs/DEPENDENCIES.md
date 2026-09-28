# Dependencies

Standard library first. Every third-party module, why it is needed, and what it may
touch. Additions require asking the user (CLAUDE.md §14). Versions are pinned in
`go.mod`; `govulncheck` runs in CI.

| Module | Version | Used by | Why | Allowed to touch |
|---|---|---|---|---|
| `golang.org/x/crypto` | v0.57.0 | identity, invite, handshake, session, store, media | argon2, blake2b, chacha20poly1305, curve25519 | key material, blobs |
| `golang.org/x/text` | v0.42.0 | text | NFKC normalization, case folding | peer strings (sanitized) |
| `golang.org/x/image` | v0.46.0 | media | WebP decoding, thumbnail scaling (`draw`) | image bytes after the size gates |
| `golang.org/x/sys` | v0.48.0 | store (now), transport, cmd/* (later) | `flock`/exclusive open, socket options, core-dump limits | OS handles |
| `github.com/flynn/noise` | v1.1.0 | handshake | Noise XK (small, mature; KAT-tested against its own cacophony vectors) | handshake messages only |
| `go.uber.org/goleak` | v1.3.0 | tests only | goroutine leak detection | — |
| `golang.org/x/term` | v0.46.0 | cmd/burrow | no-echo passphrase and invite prompts | the controlling tty |
| `github.com/BurntSushi/toml` | v1.6.0 | store | `config.toml` | config file only |
| `charm.land/bubbletea/v2` | v2.0.10 | ui/tui | TUI framework (v2 moved to `charm.land`) | terminal |
| `charm.land/bubbles/v2` | v2.2.1 | ui/tui | text input, viewport. Transitively pulls `github.com/atotto/clipboard` (pure Go; execs the platform clipboard tool on ctrl+v only) | terminal, clipboard on user action |
| `charm.land/lipgloss/v2` | v2.0.6 | ui/tui | styling | terminal |
| `github.com/skip2/go-qrcode` | v0.0.0-2020 (frozen) | cmd/burrow | `id --qr`, `invite --qr` | stdout |

Planned (allowlisted, not yet imported): `golang.org/x/image` (WebP decode, Phase 2),
`golang.org/x/term` (no-echo prompts, Phase 1), `golang.org/x/net/dns/dnsmessage` (mDNS,
Phase 4), Bubble Tea v2 + lipgloss v2 + bubbles v2 (`github.com/charmbracelet/*/v2`,
Phase 1), `fyne.io/fyne/v2` (Phase 3), `github.com/skip2/go-qrcode` (frozen; Phase 1/3),
`github.com/BurntSushi/toml` (config, Phase 1), `github.com/cretz/bine` (frozen; Phase 4),
`github.com/awnumar/memguard` (Phase 5, ask first).

Lint tools (installed by `make tools`, not linked into the binary): staticcheck v0.8.1,
gosec v2.29.0, govulncheck v1.8.0, golangci-lint v2.14.0.
