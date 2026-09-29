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
// creation are guarded by e.mu unless owned by the transfer's own goroutine.
type transfer struct {
	id       TransferID
	peer     PeerID
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
	prep media.Prepared

	// incoming
	partPath string
	destDir  string
	chunkCh  chan []byte
	doneCh   chan struct{}
	timer    *time.Timer

	accepted bool
	lastProg time.Time
	stop     chan struct{} // closed when the transfer ends (file-writer exits)
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

func (e *Engine) livePeer(id PeerID) (*peer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.contacts[id]
	if !ok {
		return nil, ErrUnknownContact
	}
	if c.Blocked {
		return nil, ErrBlocked
	}
	p := e.peers[id]
	if p == nil {
		return nil, ErrOffline
	}
	return p, nil
}

// SendImage strips and offers an image to a connected contact. Pass 1
// (metadata strip + hash) runs on a file-reader goroutine; the returned id is
// valid immediately.
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
	t := &transfer{peer: id, outgoing: true, caption: cap}
	if _, err := randomFill(t.id[:]); err != nil {
		return TransferID{}, err
	}
	e.mu.Lock()
	e.transfers[t.id] = t
	e.mu.Unlock()
	e.wg.Add(1)
	go func() { // file-reader, pass 1
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
		e.mu.Lock()
		t.prep, t.size, t.hash, t.format = prep, prep.Size, prep.Hash, prep.Format
		t.width, t.height = uint32(prep.Width), uint32(prep.Height)          // #nosec G115 -- gated ≤ 16384
		t.chunks = uint32((prep.Size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- Size ≤ limit from Prepare
		t.stream = stream
		p.transfers[stream] = t
		e.mu.Unlock()
		offer, err := wire.AppendImgOffer(nil, wire.ImgOffer{Size: prep.Size, Format: prep.Format, Width: t.width, Height: t.height, Hash: prep.Hash, Caption: []byte(cap)})
		if err != nil {
			e.failTransfer(t, "bad offer")
			return
		}
		if err := p.s.SendStream(e.sessCtx, stream, wire.TypeImgOffer, offer); err != nil {
			e.failTransfer(t, "connection lost")
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

// failTransfer removes a transfer and emits TransferFailed.
func (e *Engine) failTransfer(t *transfer, reason string) {
	e.mu.Lock()
	if e.transfers[t.id] != t {
		e.mu.Unlock()
		return // already finished
	}
	delete(e.transfers, t.id)
	if p := e.peers[t.peer]; p != nil {
		delete(p.transfers, t.stream)
	}
	if t.timer != nil {
		t.timer.Stop()
	}
	if t.stop != nil {
		close(t.stop)
	}
	e.mu.Unlock()
	e.emit(TransferFailed{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Reason: reason})
}

func (e *Engine) finishTransfer(t *transfer, path string) {
	e.mu.Lock()
	if e.transfers[t.id] != t {
		e.mu.Unlock()
		return
	}
	delete(e.transfers, t.id)
	if p := e.peers[t.peer]; p != nil {
		delete(p.transfers, t.stream)
	}
	e.mu.Unlock()
	e.emit(TransferDone{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Path: path})
}

// progress emits TransferProgress at most 4× per second (always at the end).
func (e *Engine) progress(t *transfer, done uint64) {
	now := e.now()
	if done < t.size && now.Sub(t.lastProg) < progressEvery {
		return
	}
	t.lastProg = now
	e.emit(TransferProgress{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Done: done, Size: t.size})
}

// --- sender side ---

// onAccept starts pass 2 on a file-reader goroutine.
func (e *Engine) onAccept(p *peer, t *transfer, start uint32) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
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
	}()
}

// --- receiver side ---

// onOffer gates an incoming IMG_OFFER and emits ImageOffered (or rejects it).
func (e *Engine) onOffer(p *peer, stream uint16, o wire.ImgOffer) {
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
	t := &transfer{peer: p.s.Peer(), stream: stream, size: o.Size, hash: o.Hash, format: o.Format, width: o.Width, height: o.Height, caption: caption}
	if _, err := randomFill(t.id[:]); err != nil {
		reject(wire.RejectUnsupported)
		return
	}
	t.chunks = uint32((o.Size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- Size ≤ maxImage checked above
	e.mu.Lock()
	e.transfers[t.id] = t
	p.transfers[stream] = t
	verified := e.contacts[t.peer] != nil && e.contacts[t.peer].Verified
	tid := t.id
	t.timer = time.AfterFunc(wire.AcceptPromptTimeout, func() { _ = e.RejectImage(tid) })
	e.mu.Unlock()
	e.emit(ImageOffered{Peer: t.peer, ID: t.id, Size: o.Size, Format: o.Format, Width: o.Width, Height: o.Height, Caption: caption})
	if e.cfg.AutoAcceptFromVerified && verified {
		_ = e.AcceptImage(t.id, "")
	}
}

// AcceptImage accepts an offered image into destDir ("" → the image dir),
// resuming a matching partial download when one exists.
func (e *Engine) AcceptImage(id TransferID, destDir string) error {
	e.mu.Lock()
	t := e.transfers[id]
	if t == nil {
		e.mu.Unlock()
		return ErrUnknownTransfer
	}
	if t.outgoing || t.accepted {
		e.mu.Unlock()
		return ErrNotOffered
	}
	p := e.peers[t.peer]
	t.accepted = true
	if t.timer != nil {
		t.timer.Stop()
	}
	e.mu.Unlock()
	if p == nil {
		e.failTransfer(t, "connection lost")
		return ErrOffline
	}
	if destDir == "" {
		destDir = e.imageDir()
	}
	t.destDir = destDir
	for _, d := range []string{destDir, e.partialsPath()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			e.failTransfer(t, "cannot create directory")
			return err
		}
	}
	if free, err := freeSpace(e.partialsPath()); err == nil && free < t.size+wire.FreeSpaceMargin {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgReject, wire.AppendImgReject(nil, wire.RejectTooLarge))
		e.failTransfer(t, "not enough free disk space")
		return ErrNoSpace
	}
	// Resume a matching partial (same peer and hash): truncate to whole chunks,
	// re-hash from the start, continue from there.
	start := uint32(0)
	if m, ok := e.findResumable(t.peer, t.hash); ok && m.Size == t.size {
		old := filepath.Join(e.partialsPath(), m.ID+".part")
		if err := truncatePartial(old, m.Complete); err == nil && m.Complete < t.chunks {
			e.mu.Lock()
			delete(e.transfers, t.id)
			delete(p.transfers, t.stream)
			oldID, _ := hex.DecodeString(m.ID)
			copy(t.id[:], oldID) // keep the on-disk names valid
			e.transfers[t.id] = t
			p.transfers[t.stream] = t
			e.mu.Unlock()
			start, t.partPath = m.Complete, old
			e.emit(TransferResumed{ID: t.id})
		} else {
			_ = os.Remove(old)
			_ = e.st.DeleteBlob(metaBlobPrefix + m.ID + ".meta")
		}
	}
	if t.partPath == "" {
		t.partPath = filepath.Join(e.partialsPath(), hex.EncodeToString(t.id[:])+".part")
		f, err := os.OpenFile(t.partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			e.failTransfer(t, "cannot create partial file")
			return err
		}
		_ = f.Close()
	}
	if err := e.writeMeta(t, start); err != nil {
		e.failTransfer(t, "cannot write transfer metadata")
		return err
	}
	t.chunkCh = make(chan []byte, fileChunkQueue)
	t.doneCh = make(chan struct{}, 1)
	t.stop = make(chan struct{})
	if err := p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgAccept, wire.AppendImgAccept(nil, start)); err != nil {
		e.failTransfer(t, "connection lost")
		return err
	}
	e.wg.Add(1)
	go e.fileWriter(p, t, start)
	return nil
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
	defer e.wg.Done()
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
			// The pump queues DONE after the last chunk; drain what is still buffered.
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
			e.finishTransfer(t, dest)
			return
		case <-t.stop: // transfer failed (connection lost, cancel): keep an accurate .meta for resume
			_ = f.Sync()
			if e.transfers[t.id] == nil && t.partPath != "" {
				if _, err := os.Stat(t.partPath); err == nil {
					_ = e.writeMeta(t, next)
				}
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
	e.mu.Lock()
	t := e.transfers[id]
	if t == nil {
		e.mu.Unlock()
		return ErrUnknownTransfer
	}
	if t.outgoing || t.accepted {
		e.mu.Unlock()
		return ErrNotOffered
	}
	p := e.peers[t.peer]
	e.mu.Unlock()
	if p != nil {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgReject, wire.AppendImgReject(nil, wire.RejectDeclined))
	}
	e.failTransfer(t, "declined")
	return nil
}

// CancelTransfer aborts a transfer in either direction. An incoming partial is
// deleted (a user cancel is not a resume candidate).
func (e *Engine) CancelTransfer(id TransferID) error {
	e.mu.Lock()
	t := e.transfers[id]
	if t == nil {
		e.mu.Unlock()
		return ErrUnknownTransfer
	}
	p := e.peers[t.peer]
	accepted, outgoing := t.accepted, t.outgoing
	e.mu.Unlock()
	if !accepted && !outgoing {
		return e.RejectImage(id)
	}
	if p != nil {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser))
	}
	e.failTransfer(t, "cancelled")
	e.dropPartial(t)
	return nil
}

func (e *Engine) dropPartial(t *transfer) {
	if t.partPath != "" {
		_ = os.Remove(t.partPath)
	}
	_ = e.st.DeleteBlob(e.metaRel(t.id))
}

// onStreamInbound routes IMG_* frames for one peer (called from the peer pump).
func (e *Engine) onStreamInbound(p *peer, in session.Inbound) {
	if in.Type == wire.TypeImgOffer {
		if o, err := wire.DecodeImgOffer(in.Payload); err == nil {
			e.onOffer(p, in.Stream, o)
		}
		return
	}
	e.mu.Lock()
	t := p.transfers[in.Stream]
	if t == nil {
		e.mu.Unlock()
		return
	}
	accepted := t.accepted
	if in.Type == wire.TypeImgAccept && t.outgoing && !accepted {
		t.accepted = true
	}
	e.mu.Unlock()
	switch in.Type {
	case wire.TypeImgAccept:
		start, _ := wire.DecodeImgAccept(in.Payload)
		if t.outgoing && !accepted {
			e.onAccept(p, t, start)
		}
	case wire.TypeImgReject:
		r, _ := wire.DecodeImgReject(in.Payload)
		e.failTransfer(t, "rejected: "+rejectReason(r))
	case wire.TypeImgChunk:
		c, err := wire.DecodeImgChunk(in.Payload)
		if err != nil || t.chunkCh == nil {
			return
		}
		select {
		case t.chunkCh <- c.Data:
		case <-e.sessCtx.Done():
		}
	case wire.TypeImgDone:
		if t.doneCh != nil {
			select {
			case t.doneCh <- struct{}{}:
			default:
			}
		}
	case wire.TypeImgResult:
		r, _ := wire.DecodeImgResult(in.Payload)
		if r == wire.ResultOK {
			e.finishTransfer(t, t.prep.Path)
		} else {
			e.failTransfer(t, "peer reported "+resultReason(r))
		}
	case wire.TypeImgCancel:
		e.failTransfer(t, "cancelled by peer") // an accepted partial stays on disk for resume
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

// failPeerTransfers marks every transfer of a peer failed when its session
// ends. Accepted incoming partials stay on disk with their .meta for resume.
func (e *Engine) failPeerTransfers(p *peer) {
	e.mu.Lock()
	ts := make([]*transfer, 0, len(p.transfers))
	for _, t := range p.transfers {
		ts = append(ts, t)
	}
	e.mu.Unlock()
	for _, t := range ts {
		e.failTransfer(t, "connection lost")
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
	p.close(wire.ByeResourceLimit)
	e.mu.Lock()
	c, ok := e.contacts[id]
	canDial := ok && !c.Blocked && len(c.Addrs) > 0
	e.mu.Unlock()
	if canDial && e.ctx.Err() == nil {
		e.scheduleReconnect(id)
	}
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
