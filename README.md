# Burrow

Peer-to-peer, end-to-end-encrypted, one-to-one chat for text and images. No servers, no
accounts, no third parties. Written in Go.

**Status: Phase 0 (skeleton).** The engine's wire codec, sanitization, identity, invites
and encrypted store exist and are tested; there is no working chat yet. See
`docs/STATUS.md` for the current step and `CLAUDE.md` for the full design.

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
