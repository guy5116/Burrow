package core

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/guy5116/burrow/internal/media"
	"github.com/guy5116/burrow/internal/session"
	"github.com/guy5116/burrow/internal/text"
	"github.com/guy5116/burrow/internal/wire"
)

// Transfer errors.
var (
	ErrUnknownTransfer = errors.New("core: unknown transfer")
	ErrNotOffered      = errors.New("core: transfer is not awaiting a decision")
	ErrPeerNoImages    = errors.New("core: peer does not accept images")
	ErrNoSpace         = errors.New("core: not enough free disk space")
	ErrOffline         = errors.New("core: contact is not connected")
)

const (
	metaEvery       = 16 // chunks between .meta rewrites
	progressEvery   = 250 * time.Millisecond
	fileChunkQueue  = 4
	partialsDir     = "partials"
	metaBlobPrefix  = "partials/"
	metaBlobVersion = 1
)

// transfer is one image transfer in either direction. Fields written after
// creation belong to the engine goroutine, except lastProg, which belongs to
// the transfer's own file-reader or file-writer goroutine.
type transfer struct {
	id       TransferID
	peer     PeerID
	owner    *peer // the session record the stream lives on
	stream   uint16
	outgoing bool
	size     uint64
	hash     [wire.HashSize]byte
	format   uint8
	width    uint32
	height   uint32
	caption  string
	chunks   uint32

	// outgoing
	prep     media.Prepared
	acceptCh chan uint32 // start chunk from IMG_ACCEPT, engine → file-reader

	// incoming
	partPath string
	destDir  string
	chunkCh  chan []byte
	doneCh   chan struct{}
	timer    *time.Timer

	accepted bool
	lastProg time.Time
	stop     chan struct{} // closed when the transfer fails
}

// metaRec is the encrypted sidecar for a partial download (§6.3).
type metaRec struct {
	Version  int    `json:"v"`
	ID       string `json:"id"`
	Peer     string `json:"peer"`
	Size     uint64 `json:"size"`
	Hash     string `json:"hash"`
	Format   uint8  `json:"format"`
	Dest     string `json:"dest"`
	Complete uint32 `json:"complete"`
}

func (e *Engine) partialsPath() string { return filepath.Join(e.cfg.DataDir, partialsDir) }

func (e *Engine) imageDir() string {
	if e.cfg.ImageDir != "" {
		return e.cfg.ImageDir
	}
	return filepath.Join(e.cfg.DataDir, "images")
}

// cleanPartials deletes .part files without a valid .meta and metas whose
// contact no longer exists (startup, §6.3 step 4).
func (e *Engine) cleanPartials() {
	dir := e.partialsPath()
	metas, _ := e.st.ListBlobs(partialsDir)
	valid := map[string]bool{}
	for _, rel := range metas {
		m, err := e.readMeta(rel)
		if err != nil {
			_ = e.st.DeleteBlob(rel)
			continue
		}
		pid, err := hex.DecodeString(m.Peer)
		var peer PeerID
		copy(peer[:], pid)
		if err != nil || len(pid) != 32 || e.contacts[peer] == nil {
			_ = e.st.DeleteBlob(rel)
			_ = os.Remove(filepath.Join(dir, m.ID+".part"))
			continue
		}
		valid[m.ID] = true
	}
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	for _, p := range parts {
		if id := strings.TrimSuffix(filepath.Base(p), ".part"); !valid[id] {
			_ = os.Remove(p)
		}
	}
}

func (e *Engine) readMeta(rel string) (metaRec, error) {
	b, err := e.st.ReadBlob(rel)
	if err != nil {
		return metaRec{}, err
	}
	var m metaRec
	if err := json.Unmarshal(b, &m); err != nil || m.Version != metaBlobVersion {
		return metaRec{}, errors.New("core: bad meta")
	}
	return m, nil
}

func (e *Engine) metaRel(id TransferID) string {
	return metaBlobPrefix + hex.EncodeToString(id[:]) + ".meta"
}

func (e *Engine) writeMeta(t *transfer, complete uint32) error {
	m := metaRec{Version: metaBlobVersion, ID: hex.EncodeToString(t.id[:]), Peer: hex.EncodeToString(t.peer[:]),
		Size: t.size, Hash: hex.EncodeToString(t.hash[:]), Format: t.format, Dest: t.destDir, Complete: complete}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return e.st.WriteBlob(e.metaRel(t.id), b)
}

// findResumable looks for a partial download of the same (peer, hash).
func (e *Engine) findResumable(peer PeerID, hash [wire.HashSize]byte) (metaRec, bool) {
	metas, _ := e.st.ListBlobs(partialsDir)
	for _, rel := range metas {
		m, err := e.readMeta(rel)
		if err != nil {
			continue
		}
		if m.Peer == hex.EncodeToString(peer[:]) && m.Hash == hex.EncodeToString(hash[:]) {
			return m, true
		}
	}
	return metaRec{}, false
}

func (e *Engine) livePeer(id PeerID) (p *peer, err error) {
	e.do(func() {
		c, ok := e.contacts[id]
		switch {
		case !ok:
			err = ErrUnknownContact
		case c.Blocked:
			err = ErrBlocked
		case e.peers[id] == nil:
			err = ErrOffline
		default:
			p = e.peers[id]
		}
	})
	return p, err
}

// SendImage strips and offers an image to a connected contact. Both stripping
// passes run on one file-reader goroutine; the returned id is valid
// immediately.
func (e *Engine) SendImage(ctx context.Context, id PeerID, path string, caption string) (TransferID, error) {
	p, err := e.livePeer(id)
	if err != nil {
		return TransferID{}, err
	}
	ph := p.s.PeerHello()
	if ph.Features&wire.FeatureImages == 0 {
		return TransferID{}, ErrPeerNoImages
	}
	cap, err := text.Sanitize([]byte(caption), text.SingleLine)
	if err != nil {
		return TransferID{}, err
	}
	if len(cap) > wire.MaxCaptionBytes {
		return TransferID{}, errors.New("core: caption too long")
	}
	limit := min(e.cfg.maxImage(), ph.MaxImage)
	mode := media.ModeStrip
	if e.cfg.Paranoid {
		mode = media.ModeParanoid
	}
	t := &transfer{peer: id, outgoing: true, caption: cap, acceptCh: make(chan uint32, 1), stop: make(chan struct{})}
	if _, err := randomFill(t.id[:]); err != nil {
		return TransferID{}, err
	}
	e.do(func() { e.transfers[t.id] = t })
	e.wg.Add(1)
	go func() { // file-reader
		defer e.wg.Done()
		prep, err := media.Prepare(path, mode, limit)
		if err == nil && prep.Animated && ph.Features&wire.FeatureAnimatedGIF == 0 {
			err = errAnimatedUnsupported
		}
		if err != nil {
			e.failTransfer(t, redactMediaErr(err))
			return
		}
		stream, err := p.s.OpenStream(prep.Size)
		if errors.Is(err, session.ErrStreamsExhaust) {
			e.failTransfer(t, "session ran out of stream ids; reconnecting")
			e.onStreamsExhausted(p)
			return
		}
		if err != nil {
			e.failTransfer(t, "too many transfers in progress")
			return
		}
		live := false
		e.do(func() {
			if e.transfers[t.id] != t {
				return // cancelled during pass 1
			}
			if e.bySession[p.s] != p {
				e.failTransferL(t, "connection lost")
				return
			}
			t.prep, t.size, t.hash, t.format = prep, prep.Size, prep.Hash, prep.Format
			t.width, t.height = uint32(prep.Width), uint32(prep.Height)          // #nosec G115 -- gated ≤ 16384
			t.chunks = uint32((prep.Size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- Size ≤ limit from Prepare
			t.stream, t.owner = stream, p
			p.transfers[stream] = t
			live = true
		})
		if !live {
			return
		}
		offer, err := wire.AppendImgOffer(nil, wire.ImgOffer{Size: prep.Size, Format: prep.Format, Width: t.width, Height: t.height, Hash: prep.Hash, Caption: []byte(cap)})
		if err != nil {
			e.failTransfer(t, "bad offer")
			return
		}
		if err := p.s.SendStream(e.sessCtx, stream, wire.TypeImgOffer, offer); err != nil {
			e.failTransfer(t, "connection lost")
			return
		}
		select {
		case start := <-t.acceptCh:
			e.streamOut(p, t, start)
		case <-t.stop:
		case <-e.sessCtx.Done():
		}
	}()
	return t.id, nil
}

var errAnimatedUnsupported = errors.New("peer does not accept animated GIFs")

func redactMediaErr(err error) string {
	switch {
	case errors.Is(err, media.ErrUnsupported):
		return "unsupported image format"
	case errors.Is(err, media.ErrTooLarge):
		return "image too large"
	case errors.Is(err, media.ErrAnimated):
		return "animated WebP is not supported"
	case errors.Is(err, media.ErrCorrupt):
		return "image file is malformed"
	case errors.Is(err, media.ErrMismatch):
		return "file extension does not match the image data"
	case errors.Is(err, errAnimatedUnsupported):
		return err.Error()
	case errors.Is(err, os.ErrNotExist):
		return "file not found"
	}
	return "could not read image"
}

// failTransfer removes a transfer and reports TransferFailed.
func (e *Engine) failTransfer(t *transfer, reason string) {
	e.do(func() { e.failTransferL(t, reason) })
}

func (e *Engine) failTransferL(t *transfer, reason string) {
	if !e.forgetL(t) {
		return // already finished
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	close(t.stop)
	e.queueL(TransferFailed{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Reason: reason})
}

// forgetL drops a transfer from the tables; false when it was already gone.
func (e *Engine) forgetL(t *transfer) bool {
	if e.transfers[t.id] != t {
		return false
	}
	delete(e.transfers, t.id)
	if t.owner != nil && t.owner.transfers[t.stream] == t {
		delete(t.owner.transfers, t.stream)
	}
	return true
}

func (e *Engine) finishTransferL(t *transfer, path string) {
	if e.forgetL(t) {
		e.queueL(TransferDone{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Path: path})
	}
}

// progress emits TransferProgress at most 4× per second (always at the end).
// It runs on the transfer's own goroutine and waits for a slow UI, which is
// how a stalled screen becomes backpressure on image chunks.
func (e *Engine) progress(t *transfer, done uint64) {
	now := e.now()
	if done < t.size && now.Sub(t.lastProg) < progressEvery {
		return
	}
	t.lastProg = now
	e.emit(TransferProgress{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Done: done, Size: t.size})
}

// --- sender side ---

// streamOut is pass 2, on the transfer's file-reader goroutine.
func (e *Engine) streamOut(p *peer, t *transfer, start uint32) {
	err := media.Stream(t.prep, func(index uint32, chunk []byte) error {
		if index < start {
			return nil
		}
		c, err := wire.AppendImgChunk(nil, wire.ImgChunk{Index: index, Data: chunk})
		if err != nil {
			return err
		}
		if err := p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgChunk, c); err != nil {
			return err
		}
		e.progress(t, min(uint64(index+1)*wire.ChunkData, t.size))
		return nil
	})
	switch {
	case errors.Is(err, session.ErrStreamState), errors.Is(err, session.ErrClosed):
		// The peer ended the stream (cancel/reject) or the session closed: the
		// inbound terminal frame or the session teardown reports the failure.
	case errors.Is(err, media.ErrDiverged):
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelHashDiverged))
		e.failTransfer(t, "file changed while sending")
	case err != nil:
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelLocalIO))
		e.failTransfer(t, "could not read image")
	default:
		if err := p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgDone, nil); err != nil {
			e.failTransfer(t, "connection lost")
		}
	}
}

// --- receiver side ---

// onOfferL gates an incoming IMG_OFFER and reports ImageOffered (or rejects
// it). REJECT is a terminal frame: the session queues it without blocking.
func (e *Engine) onOfferL(p *peer, stream uint16, o wire.ImgOffer) {
	caption, err := text.Sanitize(o.Caption, text.SingleLine)
	if err != nil {
		return
	}
	reject := func(reason uint8) {
		_ = p.s.SendStream(e.sessCtx, stream, wire.TypeImgReject, wire.AppendImgReject(nil, reason))
	}
	if o.Size > e.cfg.maxImage() {
		reject(wire.RejectTooLarge)
		return
	}
	if media.CheckDimensions(int(o.Width), int(o.Height)) != nil {
		reject(wire.RejectUnsupported)
		return
	}
	t := &transfer{peer: p.s.Peer(), owner: p, stream: stream, size: o.Size, hash: o.Hash, format: o.Format, width: o.Width,
		height: o.Height, caption: caption, chunkCh: make(chan []byte, fileChunkQueue), doneCh: make(chan struct{}, 1), stop: make(chan struct{})}
	if _, err := randomFill(t.id[:]); err != nil {
		reject(wire.RejectUnsupported)
		return
	}
	t.chunks = uint32((o.Size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- Size ≤ maxImage checked above
	e.transfers[t.id] = t
	p.transfers[stream] = t
	e.queueL(ImageOffered{Peer: t.peer, ID: t.id, Size: o.Size, Format: o.Format, Width: o.Width, Height: o.Height, Caption: caption})
	if c := e.contacts[t.peer]; e.cfg.AutoAcceptFromVerified && c != nil && c.Verified {
		t.accepted = true
		e.wg.Add(1)
		go func() { // this transfer's file-writer
			defer e.wg.Done()
			if start, err := e.prepareAccept(p, t, ""); err == nil {
				e.fileWriter(p, t, start)
			}
		}()
		return
	}
	tid := t.id
	t.timer = time.AfterFunc(wire.AcceptPromptTimeout, func() { _ = e.RejectImage(tid) })
}

// claimOffer finds an offer that is still awaiting a decision.
func (e *Engine) claimOffer(id TransferID, accept bool) (t *transfer, err error) {
	e.do(func() {
		t = e.transfers[id]
		switch {
		case t == nil:
			err = ErrUnknownTransfer
		case t.outgoing || t.accepted:
			err = ErrNotOffered
		default:
			t.accepted = accept
			if t.timer != nil {
				t.timer.Stop()
			}
		}
	})
	return t, err
}

// AcceptImage accepts an offered image into destDir ("" → the image dir),
// resuming a matching partial download when one exists.
func (e *Engine) AcceptImage(id TransferID, destDir string) error {
	t, err := e.claimOffer(id, true)
	if err != nil {
		return err
	}
	start, err := e.prepareAccept(t.owner, t, destDir)
	if err != nil {
		return err
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.fileWriter(t.owner, t, start)
	}()
	return nil
}

// prepareAccept does the disk work for an accepted offer and sends
// IMG_ACCEPT. It never runs on the engine goroutine.
func (e *Engine) prepareAccept(p *peer, t *transfer, destDir string) (uint32, error) {
	if destDir == "" {
		destDir = e.imageDir()
	}
	for _, d := range []string{destDir, e.partialsPath()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			e.failTransfer(t, "cannot create directory")
			return 0, err
		}
	}
	if free, err := freeSpace(e.partialsPath()); err == nil && free < t.size+wire.FreeSpaceMargin {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgReject, wire.AppendImgReject(nil, wire.RejectTooLarge))
		e.failTransfer(t, "not enough free disk space")
		return 0, ErrNoSpace
	}
	// Resume a matching partial (same peer and hash): truncate to whole chunks,
	// re-hash from the start, continue from there.
	start, partPath, newID, resumed := uint32(0), "", t.id, false
	if m, ok := e.findResumable(t.peer, t.hash); ok && m.Size == t.size {
		old := filepath.Join(e.partialsPath(), m.ID+".part")
		oldID, _ := hex.DecodeString(m.ID)
		if err := truncatePartial(old, m.Complete); err == nil && m.Complete < t.chunks && len(oldID) == len(newID) {
			copy(newID[:], oldID) // keep the on-disk names valid
			start, partPath, resumed = m.Complete, old, true
		} else {
			_ = os.Remove(old)
			_ = e.st.DeleteBlob(metaBlobPrefix + m.ID + ".meta")
		}
	}
	if partPath == "" {
		partPath = filepath.Join(e.partialsPath(), hex.EncodeToString(t.id[:])+".part")
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- our partials dir
		if err != nil {
			e.failTransfer(t, "cannot create partial file")
			return 0, err
		}
		_ = f.Close()
	}
	gone := false
	e.do(func() {
		if e.transfers[t.id] != t {
			gone = true // failed meanwhile (session lost)
			return
		}
		delete(e.transfers, t.id)
		t.id, t.destDir, t.partPath = newID, destDir, partPath
		e.transfers[t.id] = t
		if resumed {
			e.queueL(TransferResumed{ID: t.id})
		}
	})
	if gone {
		return 0, ErrOffline
	}
	if err := e.writeMeta(t, start); err != nil {
		e.failTransfer(t, "cannot write transfer metadata")
		return 0, err
	}
	if err := p.s.SetChunkSink(t.stream, t.chunkCh); err != nil {
		e.failTransfer(t, "connection lost")
		return 0, err
	}
	if err := p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgAccept, wire.AppendImgAccept(nil, start)); err != nil {
		e.failTransfer(t, "connection lost")
		return 0, err
	}
	return start, nil
}

// truncatePartial cuts a .part to complete whole chunks.
func truncatePartial(path string, complete uint32) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	keep := int64(complete) * wire.ChunkData
	if st.Size() < keep {
		return errors.New("short partial")
	}
	return os.Truncate(path, keep)
}

// fileWriter is the per-incoming-transfer goroutine: it writes chunks, hashes,
// rewrites the .meta every 16 chunks, and finishes the transfer on IMG_DONE.
func (e *Engine) fileWriter(p *peer, t *transfer, start uint32) {
	f, err := os.OpenFile(t.partPath, os.O_RDWR, 0o600) // #nosec G304 -- our partials dir
	if err != nil {
		e.failTransfer(t, "cannot open partial file")
		return
	}
	defer func() { _ = f.Close() }()
	h := media.NewHasher()
	if start > 0 { // re-hash the kept prefix
		buf := make([]byte, 1<<20)
		left := int64(start) * wire.ChunkData
		for left > 0 {
			n, err := f.Read(buf[:min(int64(len(buf)), left)])
			if n > 0 {
				h.Write(buf[:n])
				left -= int64(n)
			}
			if err != nil {
				e.failTransfer(t, "cannot read partial file")
				return
			}
		}
	}
	if _, err := f.Seek(int64(start)*wire.ChunkData, 0); err != nil {
		e.failTransfer(t, "cannot seek partial file")
		return
	}
	next := start
	write := func(chunk []byte) bool {
		if _, err := f.Write(chunk); err != nil {
			_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelLocalIO))
			e.failTransfer(t, "disk write failed")
			return false
		}
		h.Write(chunk)
		next++
		if e.cfg.ChunkDelay > 0 {
			time.Sleep(e.cfg.ChunkDelay)
		}
		if next%metaEvery == 0 {
			_ = f.Sync()
			_ = e.writeMeta(t, next)
		}
		e.progress(t, min(uint64(next)*wire.ChunkData, t.size))
		if e.chunkHook != nil {
			e.chunkHook(t, next)
		}
		return true
	}
	for {
		select {
		case chunk := <-t.chunkCh:
			if !write(chunk) {
				return
			}
		case <-t.doneCh:
			// The reader hands over every chunk before DONE reaches the engine; drain what is still buffered.
			for len(t.chunkCh) > 0 {
				if !write(<-t.chunkCh) {
					return
				}
			}
			_ = f.Sync()
			_ = f.Close()
			if next != t.chunks || subtle.ConstantTimeCompare(h.Sum(nil), t.hash[:]) != 1 {
				_ = os.Remove(t.partPath)
				_ = e.st.DeleteBlob(e.metaRel(t.id))
				_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgResult, wire.AppendImgResult(nil, wire.ResultHashMismatch))
				e.failTransfer(t, "hash mismatch")
				return
			}
			dest, err := e.placeImage(t)
			if err != nil {
				_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgResult, wire.AppendImgResult(nil, wire.ResultAborted))
				e.failTransfer(t, "cannot save image")
				return
			}
			_ = e.st.DeleteBlob(e.metaRel(t.id))
			_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgResult, wire.AppendImgResult(nil, wire.ResultOK))
			e.do(func() { e.finishTransferL(t, dest) })
			return
		case <-t.stop: // transfer failed (connection lost, cancel): keep an accurate .meta for resume
			_ = f.Sync()
			if _, err := os.Stat(t.partPath); err == nil { // a user cancel has deleted the partial
				_ = e.writeMeta(t, next)
			}
			return
		case <-e.sessCtx.Done():
			_ = e.writeMeta(t, next)
			return
		}
	}
}

// placeImage renames the verified .part to <dest>/img-<hash8>.<ext>, with the
// extension from the sniffed bytes and -2, -3… on collision.
func (e *Engine) placeImage(t *transfer) (string, error) {
	head := make([]byte, media.SniffLen)
	f, err := os.Open(t.partPath) // #nosec G304 -- our partials dir
	if err != nil {
		return "", err
	}
	n, _ := f.Read(head)
	_ = f.Close()
	format, err := media.Sniff(head[:n])
	if err != nil {
		return "", err
	}
	base := "img-" + hex.EncodeToString(t.hash[:4])
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dest := filepath.Join(t.destDir, name+"."+media.Ext(format))
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		if err := os.Rename(t.partPath, dest); err != nil {
			return "", err
		}
		return dest, nil
	}
	return "", errors.New("too many collisions")
}

// RejectImage declines an offer.
func (e *Engine) RejectImage(id TransferID) error {
	t, err := e.claimOffer(id, false)
	if err != nil {
		return err
	}
	_ = t.owner.s.SendStream(e.sessCtx, t.stream, wire.TypeImgReject, wire.AppendImgReject(nil, wire.RejectDeclined))
	e.failTransfer(t, "declined")
	return nil
}

// CancelTransfer aborts a transfer in either direction. An incoming partial is
// deleted (a user cancel is not a resume candidate).
func (e *Engine) CancelTransfer(id TransferID) error {
	var t *transfer
	var p *peer
	var part string
	offer := false
	e.do(func() {
		if t = e.transfers[id]; t != nil {
			p, part, offer = t.owner, t.partPath, !t.accepted && !t.outgoing
		}
	})
	if t == nil {
		return ErrUnknownTransfer
	}
	if offer {
		return e.RejectImage(id)
	}
	if p != nil {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser))
	}
	e.failTransfer(t, "cancelled")
	if part != "" {
		_ = os.Remove(part)
	}
	_ = e.st.DeleteBlob(e.metaRel(id))
	return nil
}

// onStreamInboundL routes IMG_* frames for one peer. IMG_CHUNK never comes
// through here: the reader hands chunk data to the file-writer directly.
func (e *Engine) onStreamInboundL(p *peer, in session.Inbound) {
	if in.Type == wire.TypeImgOffer {
		if o, err := wire.DecodeImgOffer(in.Payload); err == nil {
			e.onOfferL(p, in.Stream, o)
		}
		return
	}
	t := p.transfers[in.Stream]
	if t == nil {
		return
	}
	switch in.Type {
	case wire.TypeImgAccept:
		if start, err := wire.DecodeImgAccept(in.Payload); err == nil && t.outgoing && !t.accepted {
			t.accepted = true
			t.acceptCh <- start // capacity 1, sent once
		}
	case wire.TypeImgReject:
		r, _ := wire.DecodeImgReject(in.Payload)
		e.failTransferL(t, "rejected: "+rejectReason(r))
	case wire.TypeImgDone:
		if !t.outgoing {
			select {
			case t.doneCh <- struct{}{}:
			default:
			}
		}
	case wire.TypeImgResult:
		r, _ := wire.DecodeImgResult(in.Payload)
		if r == wire.ResultOK {
			e.finishTransferL(t, t.prep.Path)
		} else {
			e.failTransferL(t, "peer reported "+resultReason(r))
		}
	case wire.TypeImgCancel:
		e.failTransferL(t, "cancelled by peer") // an accepted partial stays on disk for resume
	}
}

func rejectReason(r uint8) string {
	switch r {
	case wire.RejectDeclined:
		return "declined"
	case wire.RejectTooLarge:
		return "too large"
	case wire.RejectUnsupported:
		return "unsupported"
	}
	return "busy"
}

func resultReason(r uint8) string {
	if r == wire.ResultHashMismatch {
		return "a hash mismatch"
	}
	return "an abort"
}

// failPeerTransfersL marks every transfer of a peer failed when its session
// ends. Accepted incoming partials stay on disk with their .meta for resume.
func (e *Engine) failPeerTransfersL(p *peer) {
	for _, t := range p.transfers {
		e.failTransferL(t, "connection lost")
	}
}

// DecodeImage safely loads a saved image for display, downscaled to fit
// maxSide (0 = full size). Call it off the UI thread.
func DecodeImage(path string, maxSide int) (image.Image, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path came from TransferDone
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) > wire.MaxImageDecodeBytes {
		return nil, media.ErrTooLarge
	}
	img, _, err := media.Decode(data)
	if err != nil {
		return nil, err
	}
	if maxSide > 0 {
		img = media.Thumbnail(img, maxSide)
	}
	return img, nil
}

// onStreamsExhausted ends a session whose stream ids ran out with BYE reason 4
// and reconnects (§4.3); a fresh session starts again at id 2 or 3.
func (e *Engine) onStreamsExhausted(p *peer) {
	id := p.s.Peer()
	p.s.Close(wire.ByeResourceLimit)
	e.do(func() {
		if c, ok := e.contacts[id]; ok && !c.Blocked && len(c.Addrs) > 0 && e.ctx.Err() == nil {
			e.scheduleReconnectL(id)
		}
	})
}

// ResumableTransfer describes a partial download kept on disk.
type ResumableTransfer struct {
	Peer   PeerID
	Size   uint64
	Done   uint64
	Format uint8
}

// Resumable lists partial downloads with valid metadata; they continue when
// the sender offers the same image again.
func (e *Engine) Resumable() []ResumableTransfer {
	metas, _ := e.st.ListBlobs(partialsDir)
	var out []ResumableTransfer
	for _, rel := range metas {
		m, err := e.readMeta(rel)
		if err != nil {
			continue
		}
		raw, err := hex.DecodeString(m.Peer)
		if err != nil || len(raw) != 32 {
			continue
		}
		var peer PeerID
		copy(peer[:], raw)
		out = append(out, ResumableTransfer{Peer: peer, Size: m.Size, Done: min(uint64(m.Complete)*wire.ChunkData, m.Size), Format: m.Format})
	}
	return out
}
