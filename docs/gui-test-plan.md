# GUI manual test plan (Phase 3)

Run on Linux, macOS and Windows with a fresh data directory (`burrow-gui --config D --data D`).
Every step lists what must be visible. Pair with a second instance (CLI or GUI) on the
same LAN or loopback with a different data directory.

## 1. First run
1. Launch with no store → "Welcome to Burrow" wizard. "Create identity" with empty
   passphrase → status says a passphrase is required. Mismatched passphrases → "do not match".
2. Tick "No passphrase (insecure)" → the warning sentence appears. Untick, enter a
   passphrase twice, create → main window opens, status bar shows `me <fp8>`.
3. Quit, relaunch → "Unlock Burrow" prompt. Wrong passphrase → error "wrong passphrase or
   corrupted store". Correct → main window.
4. While the GUI is open, run `burrow id` on the same data dir → the CLI reports
   "store in use". Reverse: open the CLI first, launch the GUI → "Store in use" dialog.

## 2. Invites and connecting
5. Invite → form (host, TTL, multi-use) → dialog with QR code, the `burrow1:` string and
   Copy. The string is never logged (check stderr with `--log-level debug`).
6. On the other instance: Connect → paste into the password-style field → within a few
   seconds a "New contact" dialog shows the peer's fingerprint and says UNVERIFIED; the
   contact appears in the sidebar with ● when online and no ✓.
7. Reuse the single-use invite from a third instance → "Could not connect … may be
   offline, may have blocked you, or their key may have changed".

## 3. Messaging
8. Type, Enter → the line appears as `me: …` with "… queued" then "✓ sent" then
   "✓✓ delivered". Shift+Enter inserts a newline.
9. Receive a message containing `https://example.org` → shown as plain text with a
   "Copy link" button and no "Open in browser" button. Enable "Allow opening links" in
   Settings → the button appears and shows the full URL in a confirmation first.
10. Receive text with bidi/zero-width characters → rendered with replacement characters,
    never reordering the line. A peer whose display name matches an existing contact →
    "Name collision" dialog with both fingerprints.
11. Enable typing indicators on both sides → "<nick> is typing…" in the status bar while
    the other side types; nothing is sent while the option is off.

## 4. Verification
12. Verify → 12 five-digit groups in large monospace, both fingerprints below. Compare
    with the peer's dialog: identical. "Mark as verified" → ✓ in the sidebar and
    "verified" in the status bar. "Not now" leaves UNVERIFIED (no green anywhere).

## 5. Images
13. Image button → file chooser → pick a JPEG with EXIF (photo from a phone) → the peer
    gets "Image offered" with size, format, dimensions, caption; the highlighted default
    button is Reject. Accept → status shows "receiving image: NN%", then a thumbnail
    bubble with the saved path. Drag a PNG onto the window → same flow.
14. Open the saved file with `exiftool`: no EXIF/GPS/XMP/ICC. Thumbnail click → viewer
    window; "1:1" toggles between fit and actual size.
15. Reject an offer → sender sees "transfer to <nick> failed: rejected: declined".
    Leave an offer unanswered 10 minutes → it auto-rejects.
16. Kill the receiving instance mid-transfer of a large image, relaunch, reconnect,
    re-send → the transfer completes and the file is intact (partials folder empty).

## 6. Settings and passphrase
17. Settings → change display name, port, toggles → "Settings saved". `config.toml`
    reflects them; defaults are typing off, timestamps on, auto-accept off, paranoid off,
    mDNS off, open links off.
18. Change passphrase → relaunch unlocks only with the new one. Remove passphrase
    (insecure) → relaunch opens without a prompt and the store has `master.key`.

## 7. Shutdown
19. Close the window while connected → the peer sees "disconnected: peer left" within
    2 s. No process remains; the lock is released (`burrow id` works immediately).
20. Idle with one peer for 20 minutes → still connected (rekeys happened silently:
    `--log-level debug` shows no errors).

## First start

1. With no store, the wizard shows the IP address warning below the Create button,
   fully readable without scrolling.

## Files that are not images

1. Click **File**, choose a `.zip` of a few MiB → the peer sees a "File offered" dialog
   that states the size first and the type `.zip`; Reject is the default button.
2. Accept → the conversation shows "file saved: …/file-<code>.zip". No thumbnail, no
   viewer. The saved name contains nothing of the original name.
3. Drag a `.pdf` onto the window → same flow. Drag a `.jpg` → it arrives as an image
   with a thumbnail, and its EXIF data is gone.
4. Settings → "Largest file" `1`, restart, have the peer send a 2 MiB file → the peer
   sees "too large" at once and no dialog appears here.
5. Settings → "Largest file" `0`, restart → the peer's attempt fails with "peer does
   not accept files"; images still work.
6. With "Auto-accept images from verified contacts" on and the peer verified, a file
   still opens the dialog.
