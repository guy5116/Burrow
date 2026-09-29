# Dependencies

Standard library first. Every third-party module, why it is needed, and what it may
touch. Additions require asking the user (CLAUDE.md §14). Versions are pinned in
`go.mod`; `govulncheck` runs in CI.

| Module | Version | Used by | Why | Allowed to touch |
|---|---|---|---|---|
| `golang.org/x/crypto` | v0.57.0 | identity, invite, handshake, session, store, media | argon2, blake2b, chacha20poly1305, curve25519 | key material, blobs |
| `golang.org/x/text` | v0.42.0 | text | NFKC normalization, case folding | peer strings (sanitized) |
| `golang.org/x/image` | v0.46.0 | media | WebP decoding, thumbnail scaling (`draw`) | image bytes after the size gates |
| `golang.org/x/sys` | v0.48.0 | store, core | `flock`/exclusive open, core-dump limits, free disk space | OS handles |
| `github.com/flynn/noise` | v1.1.0 | handshake | Noise XK (small, mature; KAT-tested against its own cacophony vectors) | handshake messages only |
| `github.com/cretz/bine` | v0.2.0 (frozen) | transport/tor | control a system tor: `ADD_ONION`, SOCKS dialer | tor control port, SOCKS |
| `golang.org/x/net` (`dns/dnsmessage` only) | v0.59.0 | transport/mdns | building and parsing announcement packets | multicast UDP |
| `go.uber.org/goleak` | v1.3.0 | tests only | goroutine leak detection | — |
| `golang.org/x/term` | v0.46.0 | cmd/burrow | no-echo passphrase and invite prompts | the controlling tty |
| `github.com/BurntSushi/toml` | v1.6.0 | store | `config.toml` | config file only |
| `charm.land/bubbletea/v2` | v2.0.10 | ui/tui | TUI framework (v2 moved to `charm.land`) | terminal |
| `charm.land/bubbles/v2` | v2.2.1 | ui/tui | text input, viewport. Transitively pulls `github.com/atotto/clipboard` (pure Go; execs the platform clipboard tool on ctrl+v only) | terminal, clipboard on user action |
| `charm.land/lipgloss/v2` | v2.0.6 | ui/tui | styling | terminal |
| `github.com/skip2/go-qrcode` | v0.0.0-2020 (frozen) | cmd/burrow, ui/gui | `id --qr`, `invite --qr`, invite dialog | stdout / canvas |
| `github.com/awnumar/memguard` | v0.23.0 | identity (tag `memguard` only) | locked, guarded, read-only memory for the identity scalar. Pure Go; pulls `github.com/awnumar/memcall` | the identity scalar |
| `fyne.io/fyne/v2` | v2.8.1 | ui/gui, cmd/burrow-gui (tag `gui`) | desktop GUI. Transitive: go-text/typesetting (fonts), goldmark (unused markdown), oksvg/rasterx (icons), go-locale, go-i18n, gobmp, glfw (CGo) | window, clipboard, file chooser, `OpenURL` only behind `open_links` |

Every module on the CLAUDE.md §14 allowlist is now in use; nothing is planned.

memguard was added after the user approved it (CLAUDE.md §3.6 asked for that first). It is
compiled only with `-tags memguard`; the default and release builds do not link it.

Lint tools (installed by `make tools`, not linked into the binary): staticcheck v0.8.1,
gosec v2.29.0, govulncheck v1.8.0, golangci-lint v2.14.0.
