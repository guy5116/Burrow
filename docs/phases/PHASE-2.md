# Phase 2 — Images

## Built

- `internal/media`: magic-byte sniffing; §6.1 gates on declared dimensions and decode
  footprint; GIF block pre-scan (frames ≤ 200, Σ area ≤ 40 M) before any `DecodeAll`; a
  process-wide two-decode semaphore; EXIF orientation parser (bounds-checked TIFF walk);
  streaming strippers for JPEG (segment walk, minimal 16-byte JFIF, scan copy handling
  RSTn/stuffing, progressive scans), PNG (chunk allowlist, CRC verify + recompute), WebP
  (RIFF chunk walk, VP8X flag clearing, animation rejected), GIF (extension filter keeping
  only GCE and NETSCAPE2.0, trailer cut); orientation ≠ 1 → bounded decode, rotate,
  re-encode (PNG→PNG, JPEG→JPEG q92, WebP→PNG); paranoid mode → PNG (animated GIF →
  GIF with pixels/delays only); two-pass `Prepare`/`Stream` with hash divergence check;
  thumbnails via `x/image/draw`. Fixtures are generated in-test, including a hand-assembled
  VP8L WebP (Go has no encoder). Fuzzers: `FuzzJPEGStrip`, `FuzzPNGStrip`,
  `FuzzWebPStrip`, `FuzzGIFStrip`, `FuzzGIFPrescan`.
- `internal/session`: the §4.3 stream table (parity, no reuse, last-64 closed ids), every
  IMG_* validation (chunk index/size, DONE only after all chunks, RESULT only after DONE),
  caps (4 pending offers, 2 active per direction, 16 draining, 32 busy-rejects/10 min, 16
  offers/60 s), CANCEL echo and draining, per-stream queues (cap 2) round-robined below
  control and chat, flush-on-drain. Honest transfers both ways during three rekeys;
  adversarial rows for every image case; `BenchmarkTransfer` ≈ 800 MB/s over `net.Pipe`.
- `internal/core`: `SendImage` (pass 1 on a file-reader goroutine), `AcceptImage`
  (free-space check, resume from a matching `.meta`, per-transfer file-writer goroutine
  with `.meta` rewrite every 16 chunks), `RejectImage`, `CancelTransfer`, 10-minute accept
  timeout, `auto_accept_from_verified`, startup partial cleanup, sniffed extension on save
  with `-2`, `-3`… collision suffixes, progress events ≤ 4/s, `DecodeImage` for UIs.
- UI: `/image`, `/accept`, `/reject`, `/cancel`, `/transfers`, `/images`, `/view` (Kitty or
  iTerm2 inline rendering after releasing the terminal), image events in text and JSON.
- End-to-end: two processes exchange an image; a 23 MiB transfer is killed mid-way,
  the receiver keeps `.part` + `.meta`, reconnects, and resumes after the re-offer.

## Unspecified choices

- A minimal JFIF APP0 is emitted only when the source had one (Adobe/CMYK JPEGs without
  JFIF stay JFIF-less to keep colours right).
- Paranoid mode keeps animated GIFs as GIF (pixels and delays only) because PNG cannot
  hold animation.
- The stripping pass also serves as the orientation probe (a second cheap file read)
  so pass 1 never buffers a file that does not need rotation.
- Inline rendering: on Kitty, thumbnails appear inside the conversation through Unicode
  placeholders (added in Phase 5); on iTerm2 it is on demand (`/view n`) with the
  terminal released. The saved path is always shown.
- Progress events carry the peer and direction (`TransferPeer`) so UIs can route them.
- `CancelTransfer` on an accepted incoming transfer deletes the partial (a user cancel
  is not a resume candidate); a lost connection keeps it.
- After a peer-initiated stream end the sender's file-reader stays silent and lets the
  inbound terminal frame report the failure (avoids two failure events).
- Received chunks are copied once into the inbound channel and once into the file-writer
  queue; the ≥ 90 MB/s budget holds with margin (session layer ≈ 800 MB/s).

## Deferred

- WebP re-encoding is impossible without an encoder; rotated WebP becomes PNG (per spec).
- Sixel is out of scope.

## Known issues

- The receive-side file-writer re-hashes a resumed prefix on the file-writer goroutine
  (bounded by the image size limit).
