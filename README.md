# Burrow

Peer-to-peer, end-to-end-encrypted, one-to-one chat for text and images. No servers, no
accounts, no third parties. Written in Go.

**Status: Phases 1–4 are complete.** Text and images work between `burrow` CLIs and the
`burrow-gui` desktop app over direct TCP or Tor onion services, with optional LAN
discovery and optional encrypted history. Hardening and release (Phase 5) is next. See `docs/STATUS.md`
for the current step and `CLAUDE.md` for the full design.

## Quick start (two machines on one LAN or VPN)

```
# both sides, once
burrow init                          # choose a passphrase
# the reachable side
burrow invite                        # prints a one-hour, single-use invite; share it privately
burrow listen                        # opens the chat
# the other side
burrow connect                       # prompts for the invite (no echo), then opens the chat
```

In the chat: type to send, `/image <path> [caption]` to offer a picture (EXIF and other
metadata are stripped first; the receiver must `/accept`), `/help` for commands, `/safety <contact>` to compare safety numbers
out of band and `/verify <contact>` once they match. `burrow --plain --json listen` gives a
scriptable line mode. The default port is 47337; change it with `burrow config set listen_port`.

## What it will protect

Content confidentiality (including against future quantum computers via a hybrid
X25519 + ML-KEM-768 root key), mutual authentication with pinned keys and safety
numbers, forward secrecy per session and per message, post-compromise security through
periodic static-key-bound rekeys, resistance to scanners that do not hold your key,
image metadata stripping, and encrypted local storage.

## What it will not protect

A compromised endpoint; a peer who shares what they received; your online presence from
anyone who ever held your public key; protocol fingerprinting by deep packet inspection;
traffic analysis (timing and coarse sizes); your IP address from your peer in direct-TCP
mode (use the Tor transport once it exists). Full list in `docs/THREAT_MODEL.md`.

## Connectivity and the trade-offs

| Mode | Set with | Hides your IP from the peer | Needs | Trade-off |
|---|---|---|---|---|
| Direct TCP (default) | `transport = "tcp"` | no | one side reachable: same LAN, a port forward, or a VPN such as WireGuard/Tailscale | fastest; your IP and the peer's are visible to each other and to the network path |
| Tor onion service | `transport = "tor"` (or `"both"`) plus a system `tor` binary | yes, both ways | nothing to forward; tor bootstraps in seconds to minutes | slower and higher latency; Tor sees connection timing but never content or identities |
| LAN discovery (mDNS) | `mdns = true` | n/a | same LAN | announces that *a* Burrow instance exists (random name, keyed tag); only your contacts can tell it is you. Off by default because it is a presence beacon |

Hostnames in invites work but reveal the peer's hostname to your DNS resolver; IP
literals and onion addresses are preferred. History is off by default; `history = true`
stores messages encrypted at rest in opaque files, and `burrow burn` wipes them.

## Building

```
make build        # CLI, no CGo
make build-gui    # desktop GUI (needs CGo + OpenGL/X11 dev packages)
make test         # go test -race ./...
make lint         # gofmt, vet, staticcheck, gosec, govulncheck, golangci-lint (run `make tools` once)
make docs-check   # docs/PROTOCOL.md constants match internal/wire
```
