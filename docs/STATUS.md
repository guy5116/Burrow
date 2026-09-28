# STATUS — read this first

## Current phase
Phase 0 — Skeleton (CLAUDE.md §16).

## Done
- go.mod (`github.com/guy5116/burrow`, go 1.24, toolchain go1.27.1), Makefile, .golangci.yml, CI.
- internal/secret, internal/buf, internal/wire (100% cov, 17 fuzz targets), internal/text,
  internal/identity, internal/invite, KATs in internal/handshake (Noise XK vectors, HKDF, HMAC, ML-KEM).

## Mid-flight
- internal/store: not started.

## Next step
- internal/store (header, lock, blobs, identity blob, directory-swap passphrase change, recovery, tests, fuzz),
  then docs (PROTOCOL.md + docs-check, THREAT_MODEL, SECURITY, DEPENDENCIES), lint, PHASE-0.md.

## Open questions for the user
- §0.3 placeholders adopted without confirmation (autonomous session): module path
  `github.com/guy5116/burrow` (note: the GitHub remote is spelled `Burrow`; Go module
  paths are case-sensitive, so the repo should be renamed to lowercase or the module
  path changed), default port 47337, Bubble Tea **v2** (`github.com/charmbracelet/bubbletea/v2`).
