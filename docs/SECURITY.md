# Security notes

## Reporting

Phase 5 will publish a disclosure process. Until then, open a private security advisory
on the GitHub repository.

## Memory hygiene

Every secret Burrow owns lives in an `internal/secret.Buffer` and is wiped with `defer`
as soon as it is no longer needed: identity scalar, handshake and rekey ephemerals,
ML-KEM seeds and shared secrets, `r_i`/`r_r`, DH outputs, root and chain keys, message
keys, the store master key and file keys.

What we cannot wipe:

- Copies made inside libraries: `flynn/noise` handshake and cipher states,
  `x/crypto/curve25519` (transient copies during scalar multiplication),
  `crypto/mlkem` key objects, hash states, and `argon2`'s working memory. We drop the
  references promptly; the garbage collector reclaims them on its own schedule.
- Copies made by the Go runtime: stack growth may copy a frame holding a secret, and
  values that escape to the heap are reclaimed, not zeroed.
- Swap: without `memguard` (optional, Phase 5) secrets may be paged to disk.
  `debug.FreeOSMemory()` after Argon2 returns its 64 MiB to the OS but does not scrub it.
- Secrets are never stored in Go strings (immutable, unwipeable).

Core dumps are disabled on Linux (`RLIMIT_CORE = 0`, `PR_SET_DUMPABLE = 0`) from Phase 1.

## At-rest protection

- Mode 1: master key = Argon2id(passphrase, salt) with t=3, m=64 MiB, p=4, computed once
  per process. There is no passphrase verifier; a wrong passphrase surfaces as an AEAD
  failure on the identity blob.
- Mode 2 (`--insecure-no-passphrase`): a random master key in `store/master.key`
  (`0600`). Anyone who can read the data directory can read everything. The CLI prints
  a loud warning.
- Every blob is bound to its relative path through the AEAD associated data and a
  path-specific HKDF key, so a blob copied to another name fails to open.
- On Unix, directories are `0700` and files `0600`. On Windows we rely on the per-user
  profile ACL of `%APPDATA%`; no explicit ACLs are set.
- One process at a time: `flock` on Unix, an exclusive open on Windows.

## Randomness

`crypto/rand` only, for keys, nonces, tokens, ids, padding, salts. Reconnect jitter
(Phase 1) is the one place `math/rand/v2` is permitted.

## Constant-time considerations

X25519 and ChaCha20-Poly1305 are constant-time in pure Go. Invite checksums are compared
with `crypto/subtle.ConstantTimeCompare`. Fingerprint parsing is not secret-dependent.
