# Burrow

Peer-to-peer, end-to-end-encrypted, one-to-one chat for text and images. No servers, no
accounts, no third parties. Written in Go.

**Status: Phases 1 and 2 are complete.** Text and images work between two `burrow` CLIs
with the full handshake, per-message forward secrecy, periodic rekeys, metadata-stripped
images and resumable transfers. The desktop GUI (Phase 3) and Tor (Phase 4) are next. See `docs/STATUS.md`
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

## Connectivity

Direct TCP requires that one side be reachable: same LAN, a port forward, or a VPN such
as WireGuard or Tailscale. Hostnames work but reveal the peer's hostname to your DNS
resolver; IP literals and onion addresses are preferred.

## Building

```
make build        # CLI, no CGo
make test         # go test -race ./...
make lint         # gofmt, vet, staticcheck, gosec, govulncheck, golangci-lint (run `make tools` once)
make docs-check   # docs/PROTOCOL.md constants match internal/wire
```
