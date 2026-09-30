package session

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/guy5116/burrow/internal/buf"
	"github.com/guy5116/burrow/internal/wire"
)

// Stream errors returned to the owner (never sent to the peer).
var (
	ErrStreamState    = errors.New("session: stream not in a state that allows this frame")
	ErrStreamsExhaust = errors.New("session: stream ids exhausted")
	ErrStreamLimit    = errors.New("session: too many concurrent transfers")
)

type streamState uint8

const (
	stOffered  streamState = iota + 1 // offer sent/received, not yet accepted
	stActive                          // accepted; chunks may flow
	stDone                            // IMG_DONE seen/sent; waiting for IMG_RESULT
	stDraining                        // we sent IMG_CANCEL; discarding until a terminal frame
)

// stream is one transfer (image or file) from this side's point of view.
type stream struct {
	id        uint16
	outgoing  bool // we are the sender
	offerSent bool // sender: the peer has been told about this id
	state     streamState
	size      uint64
	chunks    uint32
	next      uint32        // receiver: next expected chunk index
	queue     chan outFrame // frames for the writer (cap TransferQueueCap)
	done      chan struct{} // closed when the stream closes; late senders get ErrStreamState
	halt      chan struct{} // closed when chunks are no longer wanted (draining or closed)
	halted    bool
	sink      chan<- Chunk // receiver: where chunk data goes (set by the owner)
}

func newStream(id uint16, outgoing bool, size uint64) *stream {
	return &stream{id: id, outgoing: outgoing, state: stOffered, size: size, chunks: chunksFor(size),
		queue: make(chan outFrame, wire.TransferQueueCap), done: make(chan struct{}), halt: make(chan struct{})}
}

// stopReceiving releases a reader that is waiting to hand a chunk to the
// owner. Caller holds the table mutex.
func (s *stream) stopReceiving() {
	if !s.halted {
		s.halted = true
		close(s.halt)
	}
}

// Chunk is the data of one IMG_CHUNK in a pooled buffer. The receiver calls
// Release when it has written the data.
type Chunk struct {
	buf *[]byte
	n   int
}

// Data returns the chunk's bytes; they are valid until Release.
func (c Chunk) Data() []byte { return (*c.buf)[:c.n] }

// Release wipes the buffer and returns it to the pool.
func (c Chunk) Release() { buf.Put(c.buf) }

// streamTable is the mutex-guarded per-stream state shared by reader, writer
// and core. It holds no key material.
type streamTable struct {
	mu         sync.Mutex
	initiator  bool
	nextID     uint32
	open       map[uint16]*stream
	closed     [wire.ClosedStreamMemory]uint16
	closedPos  int
	closedN    int
	offerTimes []time.Time // incoming offers (churn)
	busyTimes  []time.Time // busy rejects we sent
	peerMin    uint32      // smallest id the peer may still offer
}

func newStreamTable(initiator bool) *streamTable {
	t := &streamTable{initiator: initiator, open: map[uint16]*stream{}}
	if initiator {
		t.nextID, t.peerMin = 2, 3
	} else {
		t.nextID, t.peerMin = 3, 2
	}
	return t
}

// peerParityOK reports whether id has the parity the peer allocates.
func (t *streamTable) peerParityOK(id uint16) bool {
	if t.initiator {
		return id%2 == 1 // responder allocates odd
	}
	return id%2 == 0
}

func (t *streamTable) wasClosed(id uint16) bool {
	for i := 0; i < t.closedN; i++ {
		if t.closed[i] == id {
			return true
		}
	}
	return false
}

func (t *streamTable) close(s *stream) {
	if _, open := t.open[s.id]; open {
		close(s.done)
	}
	s.stopReceiving()
	delete(t.open, s.id)
	t.closed[t.closedPos] = s.id
	t.closedPos = (t.closedPos + 1) % len(t.closed)
	if t.closedN < len(t.closed) {
		t.closedN++
	}
}

func (t *streamTable) count(outgoing bool, st streamState) int {
	n := 0
	for _, s := range t.open {
		if s.outgoing == outgoing && s.state == st {
			n++
		}
	}
	return n
}

// OpenStream allocates a stream id for an outgoing offer (§4.3 parity rule).
func (s *Session) OpenStream(size uint64) (uint16, error) {
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	st, err := s.streams.allocate(size)
	if err != nil {
		return 0, err
	}
	return st.id, nil
}

// allocate opens the next outgoing stream. Caller holds the table mutex.
func (t *streamTable) allocate(size uint64) (*stream, error) {
	if t.nextID > 0xFFFF {
		return nil, ErrStreamsExhaust
	}
	if t.count(true, stOffered) >= wire.MaxPendingOffers {
		return nil, ErrStreamLimit
	}
	st := newStream(uint16(t.nextID), true, size) // #nosec G115 -- checked above
	t.nextID += 2
	t.open[st.id] = st
	return st, nil
}

// Offer opens a stream for a transfer of size bytes and queues its offer
// (an IMG_OFFER or FILE_OFFER payload) in one step, so that offers reach the
// peer in the order of their ids however many transfers start at once: the
// peer refuses an id below one it has seen. It never blocks.
func (s *Session) Offer(typ wire.FrameType, size uint64, payload []byte) (uint16, error) {
	if typ != wire.TypeImgOffer && typ != wire.TypeFileOffer {
		return 0, wire.ErrStream
	}
	if !s.ready.Load() {
		return 0, ErrNotReady
	}
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	st, err := s.streams.allocate(size)
	if err != nil {
		return 0, err
	}
	if !s.queueOfferLocked(st, outFrame{typ: typ, stream: st.id, payload: append([]byte(nil), payload...)}) {
		delete(s.streams.open, st.id) // nothing was sent: the id is free again
		s.streams.nextID -= 2
		return 0, ErrStreamLimit
	}
	return st.id, nil
}

// queueOfferLocked puts a stream's offer in line for the writer. The line has
// room for every pending offer; it can only be full while the writer has yet
// to send offers of streams that were cancelled right after. Waiting for room
// is not an option under the table mutex, so the caller is told instead.
func (s *Session) queueOfferLocked(st *stream, f outFrame) bool {
	select {
	case s.offers <- f:
		st.offerSent = true
		s.wakeWriter()
		return true
	default:
		return false
	}
}

func chunksFor(size uint64) uint32 {
	return uint32((size + wire.ChunkData - 1) / wire.ChunkData) // #nosec G115 -- size ≤ wire.MaxTransferSize
}

// offerSize validates an incoming offer and returns the size it declares. An
// offer of a kind our own HELLO did not advertise is a protocol violation.
func offerSize(in wire.Inner, ours uint64) (uint64, error) {
	if in.Type == wire.TypeFileOffer {
		o, err := wire.DecodeFileOffer(in.Payload)
		if err != nil || ours&wire.FeatureFiles == 0 {
			return 0, ErrProtocol
		}
		return o.Size, nil
	}
	o, err := wire.DecodeImgOffer(in.Payload)
	if err != nil || ours&wire.FeatureImages == 0 {
		return 0, ErrProtocol
	}
	return o.Size, nil
}

// SendStream queues an IMG_* frame on its stream (strict order per stream,
// round-robined across streams below control and chat). It updates the
// stream state: REJECT/RESULT close, CANCEL starts draining (or closes when it
// answers a received CANCEL).
func (s *Session) SendStream(ctx context.Context, id uint16, typ wire.FrameType, payload []byte) error {
	return s.sendStream(ctx, outFrame{typ: typ, stream: id, payload: append([]byte(nil), payload...)})
}

// SendChunk queues one IMG_CHUNK whose encoded payload is (*b)[:n], with b
// from the buffer pool. The session owns b from here on and returns it to
// the pool, so sending a chunk allocates nothing of its size.
func (s *Session) SendChunk(ctx context.Context, id uint16, b *[]byte, n int) error {
	err := s.sendStream(ctx, outFrame{typ: wire.TypeImgChunk, stream: id, payload: (*b)[:n], pooled: b})
	if err != nil {
		buf.Put(b)
	}
	return err
}

func (s *Session) sendStream(ctx context.Context, f outFrame) error {
	if f.typ.Class() != wire.ClassTransfer {
		return wire.ErrStream
	}
	if !s.ready.Load() {
		return ErrNotReady
	}
	t := s.streams
	t.mu.Lock()
	st, ok := t.open[f.stream]
	if !ok {
		t.mu.Unlock()
		return ErrStreamState
	}
	q := st.queue
	switch f.typ {
	case wire.TypeImgOffer, wire.TypeFileOffer:
		if !st.outgoing || st.state != stOffered || st.offerSent {
			t.mu.Unlock()
			return ErrStreamState
		}
		queued := s.queueOfferLocked(st, f) // like Offer; the stream was opened by OpenStream
		t.mu.Unlock()
		if !queued {
			return ErrStreamLimit
		}
		return nil
	case wire.TypeImgAccept:
		if st.outgoing || st.state != stOffered {
			t.mu.Unlock()
			return ErrStreamState
		}
		if t.count(false, stActive) >= wire.MaxActiveTransfers {
			t.mu.Unlock()
			return ErrStreamLimit
		}
		startChunk, err := wire.DecodeImgAccept(f.payload)
		if err != nil {
			t.mu.Unlock()
			return err
		}
		st.state, st.next = stActive, startChunk
	case wire.TypeImgChunk, wire.TypeImgDone:
		if !st.outgoing || st.state != stActive {
			t.mu.Unlock()
			return ErrStreamState
		}
		if f.typ == wire.TypeImgDone {
			st.state = stDone
		}
	case wire.TypeImgReject, wire.TypeImgResult:
		if st.state == stDraining {
			t.mu.Unlock()
			return ErrStreamState
		}
		// Our own terminal frame goes out on a fresh queue, so that nothing
		// queued behind it on the closed stream can follow.
		t.close(st)
		s.queueClosedLocked(f)
		t.mu.Unlock()
		return nil
	case wire.TypeImgCancel:
		switch {
		case st.state == stDraining:
			t.mu.Unlock()
			return ErrStreamState
		case st.outgoing && !st.offerSent:
			// The peer has never heard of this stream: forget it without a frame.
			close(st.done)
			st.stopReceiving()
			delete(t.open, st.id)
			t.mu.Unlock()
			return nil
		case t.count(true, stDraining)+t.count(false, stDraining) >= wire.MaxDrainingStreams:
			t.mu.Unlock()
			s.fail(ErrProtocol) // the peer leaves our cancels unanswered (§4.3)
			return ErrStreamLimit
		}
		// Draining starts and the CANCEL is queued in one step under the lock:
		// the reader can never find the stream draining without its CANCEL.
		// Frames still queued on the old queue are dropped with it.
		st.state = stDraining
		st.stopReceiving()
		st.queue = make(chan outFrame, 1)
		st.queue <- f
		t.mu.Unlock()
		s.wakeWriter()
		return nil
	}
	t.mu.Unlock()
	select {
	case q <- f:
	case <-st.done:
		return ErrStreamState // closed while we waited (peer cancelled/rejected)
	case <-st.halt:
		return ErrStreamState // cancelled by us meanwhile
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return ErrClosed
	}
	s.wakeWriter()
	return nil
}

func (s *Session) wakeWriter() {
	select {
	case s.transferWake <- struct{}{}:
	default:
	}
}

// queueClosedLocked queues a frame for a stream that is already closed (our
// terminal frame, a CANCEL echo, a busy reject). Caller holds the table mutex.
func (s *Session) queueClosedLocked(f outFrame) {
	q := make(chan outFrame, 1)
	q <- f
	s.closedQueues[f.stream] = q
	s.wakeWriter()
}

// rrEntry is one queue the writer may take a transfer frame from.
type rrEntry struct {
	id uint16
	q  chan outFrame
}

// nextTransferFrame picks the next queued transfer frame, going round the
// streams in id order starting after the one served last, or returns false
// when none is ready. Called by the writer only.
func (s *Session) nextTransferFrame() (outFrame, bool) {
	select {
	case f := <-s.offers: // offers first, in the order of their ids
		return f, true
	default:
	}
	t := s.streams
	t.mu.Lock()
	s.rr = s.rr[:0]
	for id, st := range t.open {
		s.rr = append(s.rr, rrEntry{id, st.queue})
	}
	for id, q := range s.closedQueues { // frames still owed on streams already closed
		s.rr = append(s.rr, rrEntry{id, q})
	}
	t.mu.Unlock()
	slices.SortFunc(s.rr, func(a, b rrEntry) int { return int(a.id) - int(b.id) })
	start, _ := slices.BinarySearchFunc(s.rr, s.rrLast+1, func(e rrEntry, id uint32) int { return int(e.id) - int(id) })
	for i := range s.rr {
		e := s.rr[(start+i)%len(s.rr)]
		select {
		case f := <-e.q:
			s.rrLast = uint32(e.id)
			return f, true
		default:
		}
	}
	return outFrame{}, false
}

// onStreamFrame (reader) validates an IMG_* frame against the stream table
// and delivers it to the owner. Any violation → ErrProtocol.
func (s *Session) onStreamFrame(in wire.Inner) error {
	t := s.streams
	t.mu.Lock()
	defer t.mu.Unlock()
	now := s.now()
	st, open := t.open[in.Stream]
	if !open {
		if in.Type == wire.TypeImgCancel && t.wasClosed(in.Stream) {
			return decodeTerminal(in) // a late cancel on a recently closed stream is ignored
		}
		if in.Type != wire.TypeImgOffer && in.Type != wire.TypeFileOffer {
			return ErrProtocol
		}
		return s.onOffer(in, now)
	}
	if st.state == stDraining {
		if in.Type != wire.TypeImgReject && in.Type != wire.TypeImgResult && in.Type != wire.TypeImgCancel {
			return nil // discarded, whatever it is
		}
		if err := decodeTerminal(in); err != nil {
			return err
		}
		t.close(st)
		// Both sides cancelled at once: our CANCEL may still be queued and must
		// go out, or the peer stays draining.
		select {
		case f := <-st.queue:
			s.queueClosedLocked(f)
		default:
		}
		return nil
	}
	switch in.Type {
	case wire.TypeImgOffer, wire.TypeFileOffer:
		return ErrProtocol // id already in use
	case wire.TypeImgAccept:
		if !st.outgoing || st.state != stOffered {
			return ErrProtocol
		}
		start, err := wire.DecodeImgAccept(in.Payload)
		if err != nil || start >= st.chunks {
			return ErrProtocol
		}
		st.state = stActive
	case wire.TypeImgReject:
		if !st.outgoing || st.state != stOffered {
			return ErrProtocol
		}
		if _, err := wire.DecodeImgReject(in.Payload); err != nil {
			return ErrProtocol
		}
		t.close(st)
	case wire.TypeImgChunk:
		if st.outgoing || st.state != stActive {
			return ErrProtocol
		}
		c, err := wire.DecodeImgChunk(in.Payload)
		if err != nil || c.Index != st.next || c.Index >= st.chunks {
			return ErrProtocol
		}
		want := uint64(wire.ChunkData)
		if c.Index == st.chunks-1 {
			want = st.size - uint64(st.chunks-1)*wire.ChunkData
		}
		if uint64(len(c.Data)) != want {
			return ErrProtocol
		}
		st.next++
	case wire.TypeImgDone:
		if st.outgoing || st.state != stActive || wire.DecodeEmpty(in.Payload) != nil || st.next != st.chunks {
			return ErrProtocol
		}
		st.state = stDone
	case wire.TypeImgResult:
		if !st.outgoing || st.state != stDone {
			return ErrProtocol
		}
		if _, err := wire.DecodeImgResult(in.Payload); err != nil {
			return ErrProtocol
		}
		t.close(st)
	case wire.TypeImgCancel:
		if _, err := wire.DecodeImgCancel(in.Payload); err != nil {
			return ErrProtocol
		}
		// Reply with exactly one IMG_CANCEL and close. The echo goes out on a
		// fresh queue; anything still queued on the stream is never sent.
		t.close(st)
		s.queueClosedLocked(outFrame{typ: wire.TypeImgCancel, stream: st.id, payload: wire.AppendImgCancel(nil, wire.CancelUser)})
	default:
		return ErrProtocol
	}
	return s.deliverLocked(in)
}

// onOffer handles a new incoming IMG_OFFER (caller holds t.mu).
func (s *Session) onOffer(in wire.Inner, now time.Time) error {
	t := s.streams
	if !t.peerParityOK(in.Stream) || t.wasClosed(in.Stream) || uint32(in.Stream) < t.peerNextMin() {
		return ErrProtocol
	}
	size, err := offerSize(in, s.cfg.Hello.Features)
	if err != nil {
		return ErrProtocol
	}
	t.offerTimes = trimWindow(t.offerTimes, now, wire.OfferWindow)
	t.offerTimes = append(t.offerTimes, now)
	if len(t.offerTimes) > wire.MaxOffersPerWindow {
		return ErrProtocol
	}
	t.peerSeen(in.Stream)
	if t.count(false, stOffered) >= wire.MaxPendingOffers {
		// Busy: reject without an event; too many busy rejects → close.
		t.busyTimes = trimWindow(t.busyTimes, now, wire.BusyRejectWindow)
		t.busyTimes = append(t.busyTimes, now)
		if len(t.busyTimes) > wire.MaxBusyRejects {
			return ErrProtocol
		}
		t.markClosed(in.Stream)
		s.queueClosedLocked(outFrame{typ: wire.TypeImgReject, stream: in.Stream, payload: wire.AppendImgReject(nil, wire.RejectBusy)})
		return nil
	}
	t.open[in.Stream] = newStream(in.Stream, false, size)
	return s.deliverLocked(in)
}

// peerNextMin / peerSeen track the peer's id sequence so ids are never reused.
func (t *streamTable) peerNextMin() uint32 { return t.peerMin }

func (t *streamTable) peerSeen(id uint16) {
	if uint32(id)+2 > t.peerMin {
		t.peerMin = uint32(id) + 2
	}
}

func (t *streamTable) markClosed(id uint16) {
	t.closed[t.closedPos] = id
	t.closedPos = (t.closedPos + 1) % len(t.closed)
	if t.closedN < len(t.closed) {
		t.closedN++
	}
}

// decodeTerminal checks the payload of a REJECT, RESULT or CANCEL that
// changes nothing else: a malformed one is still a violation.
func decodeTerminal(in wire.Inner) (err error) {
	switch in.Type {
	case wire.TypeImgReject:
		_, err = wire.DecodeImgReject(in.Payload)
	case wire.TypeImgResult:
		_, err = wire.DecodeImgResult(in.Payload)
	case wire.TypeImgCancel:
		_, err = wire.DecodeImgCancel(in.Payload)
	}
	if err != nil {
		return ErrProtocol
	}
	return nil
}

// deliverLocked hands the frame to the owner (releasing the table lock while
// blocked). Chunk data on a stream with a sink goes straight to the
// transfer's file-writer in a pooled buffer, never through the engine: the
// channel is bounded, so a slow disk becomes TCP backpressure.
func (s *Session) deliverLocked(in wire.Inner) error {
	st := s.streams.open[in.Stream]
	s.streams.mu.Unlock()
	defer s.streams.mu.Lock()
	if st == nil || st.sink == nil || in.Type != wire.TypeImgChunk {
		return s.deliver(Inbound{Type: in.Type, Stream: in.Stream, Payload: append([]byte(nil), in.Payload...)})
	}
	c, err := wire.DecodeImgChunk(in.Payload)
	if err != nil {
		return ErrProtocol
	}
	chunk := Chunk{buf: buf.Get()}
	chunk.n = copy(*chunk.buf, c.Data)
	select {
	case st.sink <- chunk:
		return nil
	case <-st.halt: // the owner stopped this transfer: the chunk is not wanted
		chunk.Release()
		return nil
	case <-s.ctx.Done():
		chunk.Release()
		return ErrClosed
	}
}

// SetChunkSink routes the data of IMG_CHUNK frames on an accepted incoming
// stream to sink instead of the inbound channel. Call it before IMG_ACCEPT.
func (s *Session) SetChunkSink(id uint16, sink chan<- Chunk) error {
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	st, ok := s.streams.open[id]
	if !ok || st.outgoing {
		return ErrStreamState
	}
	st.sink = sink
	return nil
}
