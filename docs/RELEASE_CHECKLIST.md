# Pre-release security checklist (CLAUDE.md §17)

Status as of Phase 5. "Verified" means it was run in this repository and passed;
"Partial" and "Open" say exactly what is missing.

| # | Item | Status | Evidence |
|---|---|---|---|
| 1 | Every constant in §3–§4 matches `internal/wire` and `docs/PROTOCOL.md` | Verified | `make docs-check` (type-checks `internal/wire`, compares all exported constants; negative-tested) |
| 2 | `govulncheck` clean; dependencies at latest tagged versions and maintained | Verified | `make lint` runs `govulncheck`; `go list -m -u all` shows no newer tag for any allowlisted module. Frozen exceptions reviewed: `skip2/go-qrcode`, `cretz/bine` |
| 3 | All fuzzers ≥ 1 hour each with no findings | **Partial** | Every target ran 45 s locally with no findings; nightly CI runs each for 10 minutes. The 1-hour-per-target run (≈ 30 target-hours) has not been done |
| 4 | Adversarial suite green on all three OSes | **Partial** | Green on Linux locally under `-race`. macOS and Windows run in the CI matrix; no CI run has been observed from this session |
| 5 | No log line can contain content | Verified | Seven log call sites, all listed below; none takes a peer string, invite, key or message. Panic stacks are reduced to function names and file:line (`redactStack`, tested). Inbound failures log a reason category, never an address |
| 6 | Defaults audit | Verified | `TestConfig` asserts typing off, timestamps on, auto-accept off, mDNS off, history off, paranoid off, open-links off, empty display name, port 47337, transport tcp. `burrow init` requires a passphrase unless `--insecure-no-passphrase`. Invites default to 1 h, single-use (`wire.DefaultInviteTTL`, tested in core) |
| 7 | Release binary free of debug output, paths, usernames | Verified | `strings` on `burrow-linux-amd64` from a clean clone: no home directory, username or build path; `-trimpath -ldflags="-s -w -buildid="`; statically linked, stripped |
| 8 | Reproducible build from a clean clone on two machines | **Partial** | Two clean clones at different paths on one machine produce identical SHA-256 sums for all three targets. A second machine has not been used |
| 9 | README states what is and is not protected | Verified | README "What it will protect / will not protect" and the connectivity trade-off table; full list in `docs/THREAT_MODEL.md` |

## Log call sites (item 5)

| Site | Level | Fields |
|---|---|---|
| `session.spawn` | error | goroutine name, redacted stack |
| `core.serve` (handshake failed) | debug | handshake stage |
| `core.serve` (session failed) | debug | reason category |
| `core.addContactLocked` | error | store error (local paths only) |
| `core.recordHistory` | warn | store error (local paths only) |
| `core.runMDNS` ×2 | warn | local socket error |

`text.Redact` exists for the day a peer string must be logged; today nothing logs one.

## Coverage (CLAUDE.md §13.12)

`scripts/cover.sh` enforces ≥ 90 % for wire, session, handshake, media, invite, text and
≥ 80 % elsewhere under `internal/`; CI runs it. Exempt: `internal/testpeer` and
`internal/transport/conformance` (test support) and `internal/transport/tor` (needs a tor
binary and the Tor network; run `BURROW_TOR_TEST=1 go test ./internal/transport/tor`).

## Open before a release

- Run every fuzzer for ≥ 1 hour and commit any new corpus entries.
- Observe a green CI run on Linux, macOS and Windows, including `make test-gui`.
- Repeat the reproducible-build comparison on a second machine.
- Run the Tor conformance test with tor installed.
- Execute `docs/gui-test-plan.md` on all three OSes.
- Look at an inline thumbnail on a real Kitty terminal (the escape sequences are unit
  tested, the rendering has not been seen).

## Performance budgets (CLAUDE.md §11), measured on the development machine

| Metric | Budget | Measured | Where |
|---|---|---|---|
| Handshake CPU per side | < 5 ms | ≈ 1 ms for both sides | `BenchmarkHandshake` |
| Added latency for a TEXT frame | < 1 ms | ≈ 13 µs | `BenchmarkTextOverSession` − `BenchmarkTextOverRawTCP` |
| AEAD + chain throughput | ≥ 300 MB/s | ≈ 1200 MB/s | `BenchmarkSealOpen` |
| Image transfer end to end, with disk | ≥ 90 MB/s | ≈ 419 MB/s on loopback | `TestImageThroughput` |
| Heap allocations per frame | ≤ 8 | 8 | `TestSealOpenAllocs` |
| CLI RSS idle, one peer | < 30 MiB | ≈ 19 MiB | `TestFootprint` |
| CLI startup to ready | < 300 ms | ≈ 23 ms | `TestFootprint` |
| Idle CPU | < 0.5 % | ≈ 0 % | `TestFootprint` |
| Rekey cost | < 1.5 ms | ≈ 1.2 ms for both sides | `BenchmarkRekey` |
| GUI RSS idle, one peer | < 200 MiB | 172–179 MiB; ≈ 95 MiB of it is graphics driver libraries and shared buffers | `TestGUIFootprint` with `BURROW_GUI_FOOTPRINT=1` |
