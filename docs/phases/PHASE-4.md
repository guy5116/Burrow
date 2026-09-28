# Phase 4 — Privacy transports and options

## Built

- `internal/transport/tor`: onion-service transport over a system `tor` binary via
  `github.com/cretz/bine`. The ed25519 onion key lives in the encrypted identity blob
  (version 2; v1 blobs are upgraded on first use) and reaches tor only through
  `ADD_ONION` on the control port; tor runs with a temporary data directory that is
  deleted on close. `Dial` goes through tor's SOCKS port; `RateKey` is empty so the
  handshake failure limit is global (§3.4). The full Noise/hybrid layer still runs inside
  the onion connection. Conformance runs with `BURROW_TOR_TEST=1` and tor on PATH.
- `internal/transport/mdns`: opt-in LAN discovery with hand-built `dnsmessage` packets:
  unsolicited announcements every 30 s carrying a random per-session name, the port,
  and `n=<nonce> t=<tag>` with `tag = identity.MDNSTag(nonce, pubkey)`. Browsers use the
  UDP source address, never a claimed A record. No probing handshakes.
- `internal/core`: `Transports(listen, *TorOptions)`, `OnionAddress()`, mDNS announce +
  browse with per-nonce single dial and tag matching over contacts; encrypted history
  in opaque `history/seg-N` blobs with an encrypted index (`History(peer, n)`, off by
  default); `Burn()` wipes history and partials.
- Config keys `transport` (tcp | tor | both), `tor_exe`, `history`. CLI: `/invite tor`
  builds an onion invite from inside a running `burrow listen`; `/history [n]`;
  `burrow burn` uses the engine. GUI: transport and history in Settings, the invite
  dialog defaults to the onion address when tor runs, conversations prefill from history.

## Unspecified choices

- Announcements are unsolicited responses only (no queries answered); simpler and
  nothing to enumerate.
- An announcement's nonce is dialed at most once per process (4096-entry memory).
- History segments hold 256 records; `History` reads newest segments first and returns
  oldest-first. Records store the peer key, id, direction, text and unix time.
- `burrow invite --tor` on the command line is refused with an explanation: the onion
  address only exists while tor runs, so onion invites are created in the chat.
- Tor bootstrap is bounded at 3 minutes; the engine's `Start` blocks until the onion
  service is published.

## Deferred

- mDNS over IPv6 (ff02::fb) — IPv4 only for now.
- Tor conformance in CI (needs network access to the Tor network).

## Known issues

- `Engine.Start` waits for tor before returning; the UIs show nothing while tor
  bootstraps (up to a few minutes on first start).
