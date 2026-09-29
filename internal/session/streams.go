package session

import (
	"context"
	"errors"
	"sync"
	"time"

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
	id       uint16
	outgoing bool // we are the sender
	state    streamState
	size     uint64
	chunks   uint32
	next     uint32        // receiver: next expected chunk index
	queue    chan outFrame // sender/receiver frames for the writer (cap TransferQueueCap)
	done     chan struct{} // closed when the stream closes; late senders get ErrStreamState
	sink     chan<- []byte // receiver: where chunk data goes (set by the owner)
}

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
	t := s.streams
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nextID > 0xFFFF {
		return 0, ErrStreamsExhaust
	}
	if t.count(true, stOffered) >= wire.MaxPendingOffers {
		return 0, ErrStreamLimit
	}
	id := uint16(t.nextID) // #nosec G115 -- checked above
	t.nextID += 2
	st := &stream{id: id, outgoing: true, state: stOffered, size: size, chunks: chunksFor(size),
		queue: make(chan outFrame, wire.TransferQueueCap), done: make(chan struct{})}
	t.open[id] = st
	return id, nil
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
	if typ.Class() != wire.ClassTransfer {
		return wire.ErrStream
	}
	if !s.ready.Load() {
		return ErrNotReady
	}
	t := s.streams
	t.mu.Lock()
	st, ok := t.open[id]
	if !ok {
		t.mu.Unlock()
		return ErrStreamState
	}
	switch typ {
	case wire.TypeImgOffer, wire.TypeFileOffer:
		if !st.outgoing || st.state != stOffered {
			t.mu.Unlock()
			return ErrStreamState
		}
	case wire.TypeImgAccept:
		if st.outgoing || st.state != stOffered {
			t.mu.Unlock()
			return ErrStreamState
		}
		if t.count(false, stActive) >= wire.MaxActiveTransfers {
			t.mu.Unlock()
			return ErrStreamLimit
		}
		st.state = stActive
		startChunk, err := wire.DecodeImgAccept(payload)
		if err != nil {
			t.mu.Unlock()
			return err
		}
		st.next = startChunk
	case wire.TypeImgChunk, wire.TypeImgDone:
		if !st.outgoing || st.state != stActive {
			t.mu.Unlock()
			return ErrStreamState
		}
		if typ == wire.TypeImgDone {
			st.state = stDone
		}
	case wire.TypeImgReject, wire.TypeImgResult:
		if st.state == stDraining {
			t.mu.Unlock()
			return ErrStreamState
		}
		t.close(st)
	case wire.TypeImgCancel:
		if st.state == stDraining {
			t.mu.Unlock()
			return ErrStreamState
		}
		st.state = stDraining
		if t.count(true, stDraining)+t.count(false, stDraining) > wire.MaxDrainingStreams {
			t.mu.Unlock()
			return ErrStreamLimit
		}
	}
	f := outFrame{typ: typ, stream: id, payload: append([]byte(nil), payload...)}
	if _, stillOpen := t.open[id]; !stillOpen {
		// Our own terminal frame (REJECT/RESULT): it goes out on a fresh queue
		// so that nothing queued behind it on the closed stream can follow.
		q := make(chan outFrame, 1)
		q <- f
		s.closedQueues[id] = q
		t.mu.Unlock()
	} else {
		q := st.queue
		t.mu.Unlock()
		select {
		case q <- f:
		case <-st.done:
			return ErrStreamState // closed while we waited (peer cancelled/rejected)
		case <-ctx.Done():
			return ctx.Err()
		case <-s.closed:
			return ErrClosed
		}
	}
	select {
	case s.transferWake <- struct{}{}:
	default:
	}
	return nil
}

// nextTransferFrame picks the next queued transfer frame round-robin, or
// returns false when none is ready. Called by the writer only.
func (s *Session) nextTransferFrame() (outFrame, bool) {
	t := s.streams
	t.mu.Lock()
	ids := make([]uint16, 0, len(t.open))
	for id := range t.open {
		ids = append(ids, id)
	}
	// Also drain queues of streams closed while frames were still queued (REJECT/RESULT/CANCEL echoes).
	for id := range s.closedQueues {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	if len(ids) == 0 {
		return outFrame{}, false
	}
	for i := 0; i < len(ids); i++ {
		id := ids[(s.rrPos+i)%len(ids)]
		t.mu.Lock()
		var q chan outFrame
		if st, ok := t.open[id]; ok {
			q = st.queue
		} else {
			q = s.closedQueues[id]
		}
		t.mu.Unlock()
		if q == nil {
			continue
		}
		select {
		case f := <-q:
			s.rrPos = (s.rrPos + i + 1) % len(ids)
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
			return nil // late cancel on a recently closed stream: ignored
		}
		if in.Type != wire.TypeImgOffer && in.Type != wire.TypeFileOffer {
			return ErrProtocol
		}
		return s.onOffer(in, now)
	}
	if st.state == stDraining {
		if in.Type == wire.TypeImgReject || in.Type == wire.TypeImgResult || in.Type == wire.TypeImgCancel {
			t.close(st)
			// Our own CANCEL may still be queued (both sides cancelled at once): it must
			// still go out so the peer leaves draining. Anything else queued is dropped.
			for len(st.queue) > 0 {
				if f := <-st.queue; f.typ == wire.TypeImgCancel {
					q := make(chan outFrame, 1)
					q <- f
					s.closedQueues[st.id] = q
					select {
					case s.transferWake <- struct{}{}:
					default:
					}
				}
			}
		}
		return nil // discard everything else
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
		q := make(chan outFrame, 1)
		q <- outFrame{typ: wire.TypeImgCancel, stream: st.id, payload: wire.AppendImgCancel(nil, wire.CancelUser)}
		s.closedQueues[st.id] = q
		select {
		case s.transferWake <- struct{}{}:
		default:
		}
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
	if t.count(false, stOffered) >= wire.MaxPendingOffers || t.count(false, stActive)+t.count(false, stOffered) >= wire.MaxPendingOffers+wire.MaxActiveTransfers {
		// Busy: reject without an event; too many busy rejects → close.
		t.busyTimes = trimWindow(t.busyTimes, now, wire.BusyRejectWindow)
		t.busyTimes = append(t.busyTimes, now)
		if len(t.busyTimes) > wire.MaxBusyRejects {
			return ErrProtocol
		}
		t.markClosed(in.Stream)
		q := make(chan outFrame, 1)
		q <- outFrame{typ: wire.TypeImgReject, stream: in.Stream, payload: wire.AppendImgReject(nil, wire.RejectBusy)}
		s.closedQueues[in.Stream] = q
		select {
		case s.transferWake <- struct{}{}:
		default:
		}
		return nil
	}
	st := &stream{id: in.Stream, outgoing: false, state: stOffered, size: size, chunks: chunksFor(size),
		queue: make(chan outFrame, wire.TransferQueueCap), done: make(chan struct{})}
	t.open[in.Stream] = st
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

// deliverLocked hands the frame to the owner (releasing the table lock while blocked).
func (s *Session) deliverLocked(in wire.Inner) error {
	msg := Inbound{Type: in.Type, Stream: in.Stream, Payload: append([]byte(nil), in.Payload...)}
	var sink chan<- []byte
	if st := s.streams.open[in.Stream]; st != nil && in.Type == wire.TypeImgChunk {
		sink = st.sink
	}
	s.streams.mu.Unlock()
	defer s.streams.mu.Lock()
	if sink != nil {
		// Chunk data goes straight from the reader to the transfer's file-writer
		// (bounded; a slow disk becomes TCP backpressure), never through the engine.
		c, err := wire.DecodeImgChunk(msg.Payload)
		if err != nil {
			return ErrProtocol
		}
		select {
		case sink <- c.Data:
			return nil
		case <-s.ctx.Done():
			return ErrClosed
		}
	}
	return s.deliver(msg)
}

// SetChunkSink routes the data of IMG_CHUNK frames on an accepted incoming
// stream to sink instead of the inbound channel. Call it before IMG_ACCEPT.
func (s *Session) SetChunkSink(id uint16, sink chan<- []byte) error {
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	st, ok := s.streams.open[id]
	if !ok || st.outgoing {
		return ErrStreamState
	}
	st.sink = sink
	return nil
}

// StreamOpen reports whether id is still open (tests and core bookkeeping).
func (s *Session) StreamOpen(id uint16) bool {
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	_, ok := s.streams.open[id]
	return ok
}
