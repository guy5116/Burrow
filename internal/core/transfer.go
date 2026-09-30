package core

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/guy5116/burrow/internal/buf"
	"github.com/guy5116/burrow/internal/media"
	"github.com/guy5116/burrow/internal/session"
	"github.com/guy5116/burrow/internal/store"
	"github.com/guy5116/burrow/internal/text"
	"github.com/guy5116/burrow/internal/wire"
)

// Transfer errors.
var (
	ErrUnknownTransfer = errors.New("core: unknown transfer")
	ErrNotOffered      = errors.New("core: transfer is not awaiting a decision")
	ErrPeerNoImages    = errors.New("core: peer does not accept images")
	ErrPeerNoFiles     = errors.New("core: peer does not accept files")
	ErrNoSpace         = errors.New("core: not enough free disk space")
	ErrOffline         = errors.New("core: contact is not connected")
	ErrBusy            = errors.New("core: two downloads from this contact are already running; accept again when one has finished")
	ErrStopping        = errors.New("core: the engine is shutting down")
)

const (
	metaEvery       = 16 // chunks between .meta rewrites
	progressEvery   = 250 * time.Millisecond
	fileChunkQueue  = 4
	partialsDir     = "partials"
	metaBlobPrefix  = "partials/"
	metaBlobVersion = 1
)

// transfer is one transfer of an image or another file, in either direction.
// Fields written after creation belong to the engine goroutine, except
// lastProg, which belongs to the transfer's own file-reader or file-writer
// goroutine, and discard.
type transfer struct {
	id       TransferID // what the API and the events call this transfer; never changes
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
	ext      string // files only
	chunks   uint32

	// outgoing
	prep     media.Prepared
	acceptCh chan uint32 // start chunk from IMG_ACCEPT, engine → file-reader

	// incoming
	partID   TransferID // names the .part and .meta on disk: the id of the download that created them
	partPath string
	destDir  string
	chunkCh  chan session.Chunk
	doneCh   chan struct{}
	timer    *time.Timer
	discard  atomic.Bool // a user cancel: the partial is deleted, not kept for resume

	decided  bool // incoming: accepted or rejected, by the user or automatically
	accepted bool
	lastProg time.Time
	ctx      context.Context // ends when the transfer fails or finishes, or the sessions stop
	cancel   context.CancelFunc
}

// newTransfer gives t its id and a context that ends with the sessions.
func (e *Engine) newTransfer(t *transfer) (*transfer, error) {
	if _, err := randomFill(t.id[:]); err != nil {
		return nil, err
	}
	t.ctx, t.cancel = context.WithCancel(e.sessCtx)
	return t, nil
}

// metaRec is the encrypted sidecar for a partial download (§6.3).
type metaRec struct {
	Version  int    `json:"v"`
	ID       string `json:"id"`
	Peer     string `json:"peer"`
	Size     uint64 `json:"size"`
	Hash     string `json:"hash"`
	Format   uint8  `json:"format"`
	Ext      string `json:"ext,omitempty"`
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

// metaRel names the sidecar of the partial with the given part id.
func (e *Engine) metaRel(id TransferID) string {
	return metaBlobPrefix + hex.EncodeToString(id[:]) + ".meta"
}

func (e *Engine) writeMeta(t *transfer, complete uint32) error {
	m := metaRec{Version: metaBlobVersion, ID: hex.EncodeToString(t.partID[:]), Peer: hex.EncodeToString(t.peer[:]),
		Size: t.size, Hash: hex.EncodeToString(t.hash[:]), Format: t.format, Ext: t.ext, Dest: t.destDir, Complete: complete}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return e.st.WriteBlob(e.metaRel(t.partID), b)
}

// findResumable looks for a partial download of the same peer, hash and size
// that no running transfer is writing to.
func (e *Engine) findResumable(t *transfer) (m metaRec, partID TransferID, found bool) {
	metas, _ := e.st.ListBlobs(partialsDir)
	for _, rel := range metas {
		m, err := e.readMeta(rel)
		if err != nil || m.Peer != hex.EncodeToString(t.peer[:]) || m.Hash != hex.EncodeToString(t.hash[:]) || m.Size != t.size {
			continue
		}
		raw, err := hex.DecodeString(m.ID)
		if err != nil || len(raw) != len(partID) {
			continue
		}
		copy(partID[:], raw)
		if e.claimPart(t, partID) {
			return m, partID, true
		}
	}
	return metaRec{}, TransferID{}, false
}

// claimPart makes t the writer of the partial named partID unless another
// running transfer already is. The engine goroutine decides, so two accepts
// of the same image can never share a partial.
func (e *Engine) claimPart(t *transfer, partID TransferID) (ok bool) {
	e.do(func() {
		for _, other := range e.transfers {
			if other != t && other.partID == partID {
				return
			}
		}
		if e.transfers[t.id] == t {
			t.partID, ok = partID, true
		}
	})
	return ok
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

// How a file is sent: as an image the user chose to send as one, as an image
// that was given as a file, or byte for byte.
type sendKind uint8

const (
	sendImage     sendKind = iota // SendImage: stripped; the extension must match the bytes
	sendFileImage                 // SendFile, and the bytes are an image: stripped all the same
	sendRaw                       // SendFile, anything else: sent as it is
)

// SendImage strips and offers an image to a connected contact. Both stripping
// passes run on one file-reader goroutine; the returned id is valid
// immediately.
func (e *Engine) SendImage(ctx context.Context, id PeerID, path string, caption string) (TransferID, error) {
	return e.send(id, path, caption, sendImage)
}

// ErrKeepsMetadata refuses a photo or video format whose location, time and
// camera data Burrow cannot remove. SendFile with keepMetadata sends it anyway.
var ErrKeepsMetadata = errors.New("core: this photo or video format keeps its location and camera data, and Burrow cannot remove it")

// SendFile offers any file. An image in a format Burrow can clean goes the way
// of SendImage and loses its metadata, whatever its extension says. A photo
// or video format that keeps metadata Burrow cannot remove (HEIC, TIFF, RAW,
// MP4 …) is refused with ErrKeepsMetadata unless keepMetadata is set.
// Anything else is sent byte for byte, and only its extension is revealed,
// never its name.
func (e *Engine) SendFile(ctx context.Context, id PeerID, path string, caption string, keepMetadata bool) (TransferID, error) {
	head, err := readHead(path)
	if err != nil {
		return TransferID{}, err
	}
	if _, err := media.Sniff(head); err == nil {
		return e.send(id, path, caption, sendFileImage)
	}
	if kind := media.KeepsMetadata(head); kind != "" && !keepMetadata {
		return TransferID{}, fmt.Errorf("%w (%s)", ErrKeepsMetadata, kind)
	}
	return e.send(id, path, caption, sendRaw)
}

// readHead returns the first bytes of a regular file.
func readHead(path string) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- user-chosen file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("core: %s is not a file", filepath.Base(path))
	}
	head := make([]byte, 32)
	n, _ := io.ReadFull(f, head)
	return head[:n], nil
}

func (e *Engine) send(id PeerID, path, caption string, kind sendKind) (TransferID, error) {
	raw := kind == sendRaw
	p, err := e.livePeer(id)
	if err != nil {
		return TransferID{}, err
	}
	ph := p.s.PeerHello()
	switch {
	case raw && e.cfg.maxFile() == 0:
		return TransferID{}, errors.New("core: file transfer is turned off (max_file_mib is 0)")
	case raw && ph.Features&wire.FeatureFiles == 0:
		return TransferID{}, ErrPeerNoFiles
	case !raw && ph.Features&wire.FeatureImages == 0:
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
	if raw {
		limit = min(e.cfg.maxFile(), ph.MaxFile)
	}
	mode := media.ModeStrip
	if e.cfg.Paranoid {
		mode = media.ModeParanoid
	}
	t, err := e.newTransfer(&transfer{peer: id, outgoing: true, caption: cap, acceptCh: make(chan uint32, 1)})
	if err != nil {
		return TransferID{}, err
	}
	started := e.spawn(func() { e.transfers[t.id] = t }, func() { e.fileReader(p, t, path, kind, mode, limit) })
	if !started {
		t.cancel()
		return TransferID{}, ErrStopping
	}
	return t.id, nil
}

// fileReader is the per-outgoing-transfer goroutine: pass 1, the offer, and
// once the peer accepts, pass 2.
func (e *Engine) fileReader(p *peer, t *transfer, path string, kind sendKind, mode media.Mode, limit uint64) {
	raw := kind == sendRaw
	ph := p.s.PeerHello()
	var prep media.Prepared
	var err error
	switch kind {
	case sendRaw:
		prep, err = media.PrepareFile(t.ctx, path, limit)
	case sendFileImage:
		prep, err = media.PrepareByContent(t.ctx, path, mode, limit)
	default:
		prep, err = media.Prepare(t.ctx, path, mode, limit)
	}
	if err == nil && prep.Animated && ph.Features&wire.FeatureAnimatedGIF == 0 {
		err = errAnimatedUnsupported
	}
	switch {
	case err == nil:
	case kind == sendFileImage && errors.Is(err, media.ErrTooLarge):
		e.failTransfer(t, "too large for a picture: it is a picture, and pictures are always cleaned first, so it cannot go as a plain file")
		return
	default:
		e.failTransfer(t, redactMediaErr(err))
		return
	}
	typ := wire.TypeImgOffer
	offer, err := wire.AppendImgOffer(nil, wire.ImgOffer{Size: prep.Size, Format: prep.Format, Width: uint32(prep.Width), // #nosec G115 -- gated ≤ 16384
		Height: uint32(prep.Height), Hash: prep.Hash, Caption: []byte(t.caption)}) // #nosec G115 -- gated ≤ 16384
	if raw {
		typ = wire.TypeFileOffer
		offer, err = wire.AppendFileOffer(nil, wire.FileOffer{Size: prep.Size, Hash: prep.Hash, Ext: []byte(prep.Ext), Caption: []byte(t.caption)})
	}
	if err != nil {
		e.failTransfer(t, "bad offer")
		return
	}
	// The stream id is taken and the offer queued in one step, and only now
	// that nothing but the session itself can stop the offer: an id that is
	// taken and never offered would count against the pending offers for the
	// rest of the session, and offers must reach the peer in id order.
	var openErr error
	e.do(func() {
		switch {
		case e.transfers[t.id] != t:
			openErr = context.Canceled // cancelled during pass 1
		case e.bySession[p.s] != p:
			openErr = session.ErrClosed
		default:
			if t.stream, openErr = p.s.Offer(typ, prep.Size, offer); openErr != nil {
				return
			}
			t.prep, t.size, t.hash, t.format, t.ext = prep, prep.Size, prep.Hash, prep.Format, prep.Ext
			t.width, t.height = uint32(prep.Width), uint32(prep.Height)          // #nosec G115 -- gated ≤ 16384
			t.chunks = uint32((prep.Size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- Size ≤ limit from Prepare
			t.owner = p
			p.transfers[t.stream] = t
		}
	})
	switch {
	case openErr == nil:
	case errors.Is(openErr, context.Canceled):
		return
	case errors.Is(openErr, session.ErrStreamsExhaust):
		e.failTransfer(t, "session ran out of stream ids; reconnecting")
		e.onStreamsExhausted(p)
		return
	case errors.Is(openErr, session.ErrStreamLimit):
		e.failTransfer(t, "too many transfers in progress")
		return
	default:
		e.failTransfer(t, "connection lost")
		return
	}
	select {
	case start := <-t.acceptCh:
		e.streamOut(p, t, start)
	case <-t.ctx.Done():
		// Cancelled while the offer was on its way (CancelTransfer may have
		// seen no stream yet): tell the peer, or its download would wait for
		// ever. On a stream that is already closed or draining this is refused.
		if e.sessCtx.Err() == nil {
			_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser))
		}
	}
}

var errAnimatedUnsupported = errors.New("peer does not accept animated GIFs")

func redactMediaErr(err error) string {
	switch {
	case errors.Is(err, media.ErrUnsupported):
		return "unsupported image format"
	case errors.Is(err, media.ErrTooLarge):
		return "too large: over your size limit or the one your contact allows"
	case errors.Is(err, media.ErrEmpty):
		return "file is empty"
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
	t.cancel()
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
		t.cancel()
		e.queueL(TransferDone{Peer: TransferPeer{Peer: t.peer, Outgoing: t.outgoing}, ID: t.id, Path: path, Image: t.format != FormatFile})
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

// streamOut is pass 2, on the transfer's file-reader goroutine. Chunks
// travel in pooled buffers that the session returns once they are written.
func (e *Engine) streamOut(p *peer, t *transfer, start uint32) {
	err := media.Stream(t.ctx, t.prep, func(index uint32, chunk []byte) error {
		if index < start {
			return nil
		}
		b := buf.Get()
		payload, err := wire.AppendImgChunk((*b)[:0], wire.ImgChunk{Index: index, Data: chunk})
		if err != nil {
			buf.Put(b)
			return err
		}
		if err := p.s.SendChunk(t.ctx, t.stream, b, len(payload)); err != nil {
			return err
		}
		e.progress(t, min(uint64(index+1)*wire.ChunkData, t.size))
		return nil
	})
	cancel := func(reason uint8) {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, reason))
	}
	switch {
	case err == nil:
		if err := p.s.SendStream(t.ctx, t.stream, wire.TypeImgDone, nil); err != nil {
			e.failTransfer(t, "connection lost")
		}
	case errors.Is(err, session.ErrStreamState), errors.Is(err, session.ErrClosed), t.ctx.Err() != nil:
		// The peer ended the stream, the session closed, or the transfer was
		// cancelled here: whoever ended it has reported it.
	case errors.Is(err, media.ErrDiverged):
		cancel(wire.CancelHashDiverged)
		e.failTransfer(t, "file changed while sending")
	default:
		cancel(wire.CancelLocalIO)
		e.failTransfer(t, "could not read image")
	}
}

// --- receiver side ---

// onOfferL gates an incoming offer and reports ImageOffered (or rejects it).
// o.Format is FormatFile for a file offer, whose size limit is the file
// limit. REJECT is a terminal frame: the session queues it without blocking.
func (e *Engine) onOfferL(p *peer, stream uint16, o wire.ImgOffer, ext string) {
	caption, err := text.Sanitize(o.Caption, text.SingleLine)
	if err != nil {
		return
	}
	reject := func(reason uint8) {
		_ = p.s.SendStream(e.sessCtx, stream, wire.TypeImgReject, wire.AppendImgReject(nil, reason))
	}
	limit := e.cfg.maxImage()
	if o.Format == FormatFile {
		limit = e.cfg.maxFile()
	}
	if o.Size > limit { // over our limit: refused before the user is asked
		reject(wire.RejectTooLarge)
		return
	}
	if o.Format != FormatFile && media.CheckDimensions(int(o.Width), int(o.Height)) != nil {
		reject(wire.RejectUnsupported)
		return
	}
	t, err := e.newTransfer(&transfer{peer: p.s.Peer(), owner: p, stream: stream, size: o.Size, hash: o.Hash, format: o.Format,
		width: o.Width, height: o.Height, caption: caption, ext: ext,
		chunks:  uint32((o.Size + wire.ChunkData - 1) / wire.ChunkData), // #nosec G115 -- Size ≤ the limit checked above
		chunkCh: make(chan session.Chunk, fileChunkQueue), doneCh: make(chan struct{}, 1)})
	if err != nil {
		reject(wire.RejectUnsupported)
		return
	}
	e.transfers[t.id] = t
	p.transfers[stream] = t
	e.queueL(ImageOffered{Peer: t.peer, ID: t.id, Size: o.Size, Format: o.Format, Ext: ext, Width: o.Width, Height: o.Height, Caption: caption})
	// Auto-accept covers images only: a file is always the user's decision.
	// When two downloads are running the offer waits for the user instead.
	if c := e.contacts[t.peer]; e.cfg.AutoAcceptFromVerified && c != nil && c.Verified && o.Format != FormatFile && e.downloadsL(p) < wire.MaxActiveTransfers {
		t.decided, t.accepted = true, true
		if e.spawnL(func() { e.download(p, t, "") }) {
			return
		}
		t.decided, t.accepted = false, false
	}
	tid := t.id
	t.timer = time.AfterFunc(wire.AcceptPromptTimeout, func() { _ = e.RejectImage(tid) })
}

// downloadsL counts the accepted incoming transfers on p's session.
func (e *Engine) downloadsL(p *peer) (n int) {
	for _, t := range p.transfers {
		if !t.outgoing && t.accepted {
			n++
		}
	}
	return n
}

// claimOffer takes the decision on an offer that is still awaiting one, and
// marks it accepted when accept is set. Only one decision is ever taken: an
// accept and a reject that race each other cannot both go out.
func (e *Engine) claimOffer(id TransferID, accept bool) (t *transfer, err error) {
	e.do(func() {
		t = e.transfers[id]
		switch {
		case t == nil:
			err = ErrUnknownTransfer
		case t.outgoing || t.decided:
			err = ErrNotOffered
		case accept && e.downloadsL(t.owner) >= wire.MaxActiveTransfers:
			err = ErrBusy // the offer stays; the user can accept it later
		default:
			t.decided, t.accepted = true, accept
			if t.timer != nil {
				t.timer.Stop()
			}
		}
	})
	return t, err
}

// gone reports whether t has ended (failed, cancelled, finished).
func (e *Engine) gone(t *transfer) (gone bool) {
	e.do(func() { gone = e.transfers[t.id] != t })
	return gone
}

// AcceptImage accepts an offered image or file into destDir ("" → the image
// dir), resuming a matching partial download when one exists.
func (e *Engine) AcceptImage(id TransferID, destDir string) error {
	t, err := e.claimOffer(id, true)
	if err != nil {
		return err
	}
	start, err := e.prepareAccept(t.owner, t, destDir)
	if err != nil {
		return err
	}
	if !e.spawn(nil, func() { e.fileWriter(t.owner, t, start) }) {
		e.failTransfer(t, "shutting down")
		return ErrStopping
	}
	return nil
}

// download accepts and receives on one goroutine (auto-accept).
func (e *Engine) download(p *peer, t *transfer, destDir string) {
	if start, err := e.prepareAccept(p, t, destDir); err == nil {
		e.fileWriter(p, t, start)
	}
}

// prepareAccept does the disk work for an accepted offer and sends
// IMG_ACCEPT. It never runs on the engine goroutine. On failure the transfer
// has failed and the peer has been told.
func (e *Engine) prepareAccept(p *peer, t *transfer, destDir string) (start uint32, err error) {
	reject := func(wireReason uint8, reason string) {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgReject, wire.AppendImgReject(nil, wireReason))
		e.failTransfer(t, reason)
	}
	if destDir == "" {
		destDir = e.imageDir()
	}
	for _, d := range []string{destDir, e.partialsPath()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			reject(wire.RejectDeclined, "cannot create directory")
			return 0, err
		}
	}
	if free, err := store.FreeSpace(e.partialsPath()); err == nil && free < t.size+wire.FreeSpaceMargin {
		reject(wire.RejectTooLarge, "not enough free disk space")
		return 0, ErrNoSpace
	}
	// Resume a matching partial: truncate to whole chunks; the file-writer
	// hashes them again from the start.
	partPath, resumed := "", false
	if m, partID, ok := e.findResumable(t); ok {
		old := filepath.Join(e.partialsPath(), m.ID+".part")
		if err := truncatePartial(old, m.Complete); err == nil && m.Complete < t.chunks {
			start, partPath, resumed = m.Complete, old, true
		} else {
			_ = os.Remove(old)
			_ = e.st.DeleteBlob(e.metaRel(partID))
		}
	}
	if !resumed {
		if !e.claimPart(t, t.id) {
			return 0, ErrOffline // failed meanwhile (session lost)
		}
		partPath = filepath.Join(e.partialsPath(), hex.EncodeToString(t.id[:])+".part")
		f, err := os.OpenFile(partPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- our partials dir
		if err != nil {
			reject(wire.RejectDeclined, "cannot create partial file")
			return 0, err
		}
		_ = f.Close()
	}
	gone := false
	e.do(func() {
		if gone = e.transfers[t.id] != t; gone {
			return
		}
		t.destDir, t.partPath = destDir, partPath
		if resumed {
			e.queueL(TransferResumed{ID: t.id})
		}
	})
	if gone {
		return 0, ErrOffline
	}
	if err := e.writeMeta(t, start); err != nil {
		reject(wire.RejectDeclined, "cannot write transfer metadata")
		return 0, err
	}
	// A cancel that came meanwhile removed the partial, perhaps before the
	// sidecar above was written: remove what is left and stop.
	if e.gone(t) {
		e.dropPartial(t)
		return 0, ErrOffline
	}
	err = p.s.SetChunkSink(t.stream, t.chunkCh)
	if err == nil {
		err = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgAccept, wire.AppendImgAccept(nil, start))
	}
	if errors.Is(err, session.ErrStreamLimit) { // claimOffer counts the same way; kept as a backstop
		reject(wire.RejectBusy, "too many downloads at once")
		return 0, ErrBusy
	}
	if err != nil {
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
	// abort ends a transfer that failed on our side and tells the peer to stop.
	abort := func(reason string) {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelLocalIO))
		e.failTransfer(t, reason)
	}
	f, err := os.OpenFile(t.partPath, os.O_RDWR, 0o600) // #nosec G304 -- our partials dir
	if err != nil {
		abort("cannot open partial file")
		return
	}
	defer func() { _ = f.Close() }()
	h := media.NewHasher()
	kept := int64(start) * wire.ChunkData
	if n, err := io.Copy(h, io.LimitReader(f, kept)); err != nil || n != kept { // hash the kept prefix again
		abort("cannot read partial file")
		return
	}
	next := start
	write := func(c session.Chunk) bool {
		_, err := f.Write(c.Data())
		if err == nil {
			h.Write(c.Data())
		}
		c.Release()
		if err != nil {
			abort("disk write failed")
			return false
		}
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
			e.finishDownload(p, t, f, h.Sum(nil), next)
			return
		case <-t.ctx.Done(): // failed, cancelled, or shutting down
			_ = f.Sync()
			if t.discard.Load() {
				e.dropPartial(t)
			} else {
				_ = e.writeMeta(t, next) // an accurate .meta, so a later offer can resume
			}
			return
		}
	}
}

// finishDownload verifies a complete download, moves it into place and
// answers the sender.
func (e *Engine) finishDownload(p *peer, t *transfer, f *os.File, sum []byte, got uint32) {
	result := func(status uint8) {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgResult, wire.AppendImgResult(nil, status))
	}
	_ = f.Sync()
	_ = f.Close()
	if got != t.chunks || subtle.ConstantTimeCompare(sum, t.hash[:]) != 1 {
		e.dropPartial(t)
		result(wire.ResultHashMismatch)
		e.failTransfer(t, "hash mismatch")
		return
	}
	dest, err := e.placeImage(t)
	switch {
	case errors.Is(err, media.ErrUnsupported):
		e.dropPartial(t) // offered as an image, and it is none
		result(wire.ResultAborted)
		e.failTransfer(t, "the received data is not an image")
		return
	case err != nil:
		result(wire.ResultAborted) // the verified partial stays for a later attempt
		e.failTransfer(t, "cannot save the file in "+t.destDir)
		return
	}
	_ = e.st.DeleteBlob(e.metaRel(t.partID))
	result(wire.ResultOK)
	e.do(func() { e.finishTransferL(t, dest) })
}

// dropPartial deletes a download's partial and its sidecar.
func (e *Engine) dropPartial(t *transfer) {
	_ = os.Remove(t.partPath)
	_ = e.st.DeleteBlob(e.metaRel(t.partID))
}

// placeImage renames the verified .part to <dest>/img-<hash8>.<ext>, with the
// extension from the sniffed bytes and -2, -3… on collision. A file that is
// not an image becomes file-<hash8>.<ext> with the extension its offer
// carried (the wire codec admits a–z and 0–9 only), or .bin without one.
func (e *Engine) placeImage(t *transfer) (string, error) {
	if t.format == FormatFile {
		return placeAs(t, "file-"+hex.EncodeToString(t.hash[:4]), SavedExt(t.ext))
	}
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
	return placeAs(t, "img-"+hex.EncodeToString(t.hash[:4]), media.Ext(format))
}

// activeExt lists file types that can act without being opened as a document:
// programs, scripts, shortcuts (Windows fetches a .url or .lnk icon from
// wherever it points, as soon as a folder shows it), installers, and disk
// images that Windows mounts without its download warnings.
var activeExt = map[string]bool{
	"exe": true, "com": true, "scr": true, "pif": true, "cpl": true, "msi": true, "msp": true, "mst": true,
	"bat": true, "cmd": true, "ps1": true, "psm1": true, "psd1": true, "vbs": true, "vbe": true,
	"js": true, "jse": true, "wsf": true, "wsh": true, "hta": true, "reg": true, "inf": true, "chm": true,
	"url": true, "lnk": true, "scf": true, "appx": true, "msix": true, "gadget": true, "jar": true,
	"iso": true, "img": true, "vhd": true, "vhdx": true,
	"desktop": true, "command": true, "tool": true,
}

// SavedExt is the extension a received file is saved with: the one its offer
// claimed, with ".bin" added to a type that could act by itself (see
// activeExt), and "bin" when the offer claimed none.
func SavedExt(ext string) string {
	switch {
	case ext == "":
		return "bin"
	case activeExt[ext]:
		return ext + ".bin"
	}
	return ext
}

// placeAs moves the partial to <destDir>/<base>.<ext>, or <base>-2.<ext> and
// so on when that name is taken. The name is reserved before the move, so two
// downloads that finish together cannot overwrite each other.
func placeAs(t *transfer, base, ext string) (string, error) {
	for i := 1; i < 1000; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dest := filepath.Join(t.destDir, name+"."+ext)
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- name built from the hash
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_ = f.Close()
		if err := moveFile(t.partPath, dest); err != nil {
			_ = os.Remove(dest)
			return "", err
		}
		markDownloaded(dest)
		return dest, nil
	}
	return "", errors.New("too many collisions")
}

// moveFile renames src over dst, or copies it when they are on different
// file systems (rename cannot cross them).
func moveFile(src, dst string) error {
	if os.Rename(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src) // #nosec G304 -- our partials dir
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o600) // #nosec G304 -- reserved by placeAs
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Remove(src)
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
	offer, receiving := false, false
	e.do(func() {
		if t = e.transfers[id]; t != nil {
			p, offer, receiving = t.owner, !t.accepted && !t.outgoing, t.partPath != ""
		}
	})
	if t == nil {
		return ErrUnknownTransfer
	}
	if offer {
		return e.RejectImage(id)
	}
	t.discard.Store(true)
	if p != nil {
		_ = p.s.SendStream(e.sessCtx, t.stream, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser))
	}
	e.failTransfer(t, "cancelled")
	if receiving {
		e.dropPartial(t) // the file-writer does the same when it stops; whoever is last wins
	}
	return nil
}

// onStreamInboundL routes IMG_* frames for one peer. IMG_CHUNK never comes
// through here: the reader hands chunk data to the file-writer directly.
func (e *Engine) onStreamInboundL(p *peer, in session.Inbound) {
	switch in.Type {
	case wire.TypeImgOffer:
		if o, err := wire.DecodeImgOffer(in.Payload); err == nil {
			e.onOfferL(p, in.Stream, o, "")
		}
		return
	case wire.TypeFileOffer:
		if o, err := wire.DecodeFileOffer(in.Payload); err == nil {
			e.onOfferL(p, in.Stream, wire.ImgOffer{Size: o.Size, Format: FormatFile, Hash: o.Hash, Caption: o.Caption}, string(o.Ext))
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
func DecodeImage(ctx context.Context, path string, maxSide int) (image.Image, error) {
	if st, err := os.Stat(path); err != nil {
		return nil, err
	} else if uint64(st.Size()) > wire.MaxImageDecodeBytes { // #nosec G115 -- a size is not negative
		return nil, media.ErrTooLarge
	}
	if head, err := readHead(path); err != nil {
		return nil, err
	} else if _, err := media.Sniff(head); err != nil { // a received file of another kind is never read into memory
		return nil, media.ErrUnsupported
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path came from TransferDone
	if err != nil {
		return nil, err
	}
	img, _, err := media.Decode(ctx, data)
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
