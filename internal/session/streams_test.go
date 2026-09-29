package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/blake2b"

	"github.com/guy5116/burrow/internal/wire"
)

// transferOut sends size random bytes on a new stream and waits for IMG_RESULT ok.
// Frames for the sender arrive on the sender's Inbound; the receiver side is
// driven by transferIn. Both run concurrently in tests.
func transferOut(t *testing.T, s *Session, size int, inbound <-chan Inbound) {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, size)
	_, _ = rand.Read(data)
	id, err := s.OpenStream(uint64(size))
	if err != nil {
		t.Fatal(err)
	}
	offer := wire.ImgOffer{Size: uint64(size), Format: wire.FormatPNG, Width: 1, Height: 1, Hash: blake2b.Sum256(data)}
	op, _ := wire.AppendImgOffer(nil, offer)
	if err := s.SendStream(ctx, id, wire.TypeImgOffer, op); err != nil {
		t.Fatal(err)
	}
	in := <-inbound
	if in.Type != wire.TypeImgAccept || in.Stream != id {
		t.Fatalf("expected accept, got %#x on %d", in.Type, in.Stream)
	}
	for i := 0; i*wire.ChunkData < size; i++ {
		end := min((i+1)*wire.ChunkData, size)
		cp, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: uint32(i), Data: data[i*wire.ChunkData : end]})
		if err := s.SendStream(ctx, id, wire.TypeImgChunk, cp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SendStream(ctx, id, wire.TypeImgDone, nil); err != nil {
		t.Fatal(err)
	}
	in = <-inbound
	if in.Type != wire.TypeImgResult || in.Stream != id {
		t.Fatalf("expected result, got %#x", in.Type)
	}
	if r, _ := wire.DecodeImgResult(in.Payload); r != wire.ResultOK {
		t.Fatalf("result %d", r)
	}
	if s.StreamOpen(id) {
		t.Fatal("stream still open after RESULT")
	}
}

// transferIn accepts one offer, receives chunks, verifies the hash and answers RESULT ok.
func transferIn(t *testing.T, s *Session, inbound <-chan Inbound) {
	t.Helper()
	ctx := context.Background()
	in := <-inbound
	if in.Type != wire.TypeImgOffer {
		t.Fatalf("expected offer, got %#x", in.Type)
	}
	o, _ := wire.DecodeImgOffer(in.Payload)
	id := in.Stream
	if err := s.SendStream(ctx, id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); err != nil {
		t.Fatal(err)
	}
	h, _ := blake2b.New256(nil)
	for {
		in = <-inbound
		if in.Stream != id {
			t.Fatalf("frame on stream %d", in.Stream)
		}
		if in.Type == wire.TypeImgDone {
			break
		}
		if in.Type != wire.TypeImgChunk {
			t.Fatalf("got %#x", in.Type)
		}
		c, _ := wire.DecodeImgChunk(in.Payload)
		h.Write(c.Data)
	}
	status := uint8(wire.ResultOK)
	if !bytes.Equal(h.Sum(nil), o.Hash[:]) {
		status = wire.ResultHashMismatch
	}
	if err := s.SendStream(ctx, id, wire.TypeImgResult, wire.AppendImgResult(nil, status)); err != nil {
		t.Fatal(err)
	}
	if status != wire.ResultOK {
		t.Fatal("hash mismatch")
	}
}

// splitInbound routes a session's Inbound frames per stream so concurrent
// transfers in a test can each read their own frames.
func splitInbound(s *Session, out map[uint16]chan Inbound, chat chan Inbound, stop <-chan struct{}) {
	for {
		select {
		case in := <-s.Inbound():
			if in.Stream == wire.StreamChat {
				chat <- in
				continue
			}
			ch, ok := out[in.Stream]
			if !ok {
				ch = make(chan Inbound, 64)
				out[in.Stream] = ch
			}
			ch <- in
		case <-stop:
			return
		}
	}
}

// Transfers in both directions with three rekey epochs while chunks are in flight.
func TestTransfersBothWaysUnderRekey(t *testing.T) {
	p := newPair(t, time.Hour)
	const size = 3*wire.ChunkData + 12345
	// Initiator sends on stream 2, responder on stream 3.
	iIn := map[uint16]chan Inbound{2: make(chan Inbound, 64), 3: make(chan Inbound, 64)}
	rIn := map[uint16]chan Inbound{2: make(chan Inbound, 64), 3: make(chan Inbound, 64)}
	iChat, rChat := make(chan Inbound, 64), make(chan Inbound, 64)
	stop := make(chan struct{})
	go splitInbound(p.i, iIn, iChat, stop)
	go splitInbound(p.r, rIn, rChat, stop)
	defer close(stop)
	done := make(chan struct{}, 4)
	go func() { transferOut(t, p.i, size, iIn[2]); done <- struct{}{} }()
	go func() { transferIn(t, p.r, rIn[2]); done <- struct{}{} }()
	go func() { transferOut(t, p.r, size, rIn[3]); done <- struct{}{} }()
	go func() { transferIn(t, p.i, iIn[3]); done <- struct{}{} }()
	for epoch := uint32(1); epoch <= 3; epoch++ {
		time.Sleep(3 * time.Millisecond)
		_ = p.i.Rekey()
		waitEpoch(t, p.i, epoch, epoch)
		waitEpoch(t, p.r, epoch, epoch)
	}
	// Chat is not starved by the transfer.
	sendText(t, p.i, 7, "mid-transfer")
	select {
	case in := <-rChat:
		if tx, _ := wire.DecodeText(in.Payload); string(tx.Text) != "mid-transfer" {
			t.Fatal("text")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat starved")
	}
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("transfer %d timed out (i=%v r=%v)", i, p.i.Err(), p.r.Err())
		}
	}
	if p.i.Err() != nil || p.r.Err() != nil {
		t.Fatal(p.i.Err(), p.r.Err())
	}
}

func TestStreamIDParityAndLimits(t *testing.T) {
	p := newPair(t, time.Hour)
	ids := []uint16{}
	for i := 0; i < wire.MaxPendingOffers; i++ {
		id, err := p.i.OpenStream(10)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if ids[0] != 2 || ids[1] != 4 {
		t.Fatal(ids)
	}
	if _, err := p.i.OpenStream(10); !errors.Is(err, ErrStreamLimit) {
		t.Fatal(err)
	}
	if id, _ := p.r.OpenStream(10); id != 3 {
		t.Fatal(id)
	}
	if err := p.i.SendStream(context.Background(), 99, wire.TypeImgOffer, nil); !errors.Is(err, ErrStreamState) {
		t.Fatal(err)
	}
	if err := p.i.SendStream(context.Background(), ids[0], wire.TypeText, nil); !errors.Is(err, wire.ErrStream) {
		t.Fatal(err)
	}
	if err := p.i.SendStream(context.Background(), ids[0], wire.TypeImgChunk, nil); !errors.Is(err, ErrStreamState) {
		t.Fatal("chunk before accept")
	}
}

// Receiver cancels mid-transfer: the sender echoes exactly one CANCEL and both close.
func TestCancelRaces(t *testing.T) {
	p := newPair(t, time.Hour)
	ctx := context.Background()
	id, _ := p.i.OpenStream(2 * wire.ChunkData)
	op, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: 2 * wire.ChunkData, Format: 1})
	if err := p.i.SendStream(ctx, id, wire.TypeImgOffer, op); err != nil {
		t.Fatal(err)
	}
	if in := <-p.r.Inbound(); in.Type != wire.TypeImgOffer {
		t.Fatal(in.Type)
	}
	if err := p.r.SendStream(ctx, id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); err != nil {
		t.Fatal(err)
	}
	if in := <-p.i.Inbound(); in.Type != wire.TypeImgAccept {
		t.Fatal(in.Type)
	}
	// Both sides cancel at once.
	if err := p.r.SendStream(ctx, id, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser)); err != nil {
		t.Fatal(err)
	}
	if err := p.i.SendStream(ctx, id, wire.TypeImgCancel, wire.AppendImgCancel(nil, wire.CancelUser)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for p.i.StreamOpen(id) || p.r.StreamOpen(id) {
		if time.Now().After(deadline) {
			t.Fatalf("streams not closed: i=%v r=%v", p.i.StreamOpen(id), p.r.StreamOpen(id))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if p.i.Err() != nil || p.r.Err() != nil {
		t.Fatal(p.i.Err(), p.r.Err())
	}
	// Cancel after DONE: a late CANCEL on a closed stream is ignored.
	id2, _ := p.i.OpenStream(5)
	op2, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: 5, Format: 1})
	_ = p.i.SendStream(ctx, id2, wire.TypeImgOffer, op2)
	<-p.r.Inbound()
	_ = p.r.SendStream(ctx, id2, wire.TypeImgReject, wire.AppendImgReject(nil, wire.RejectDeclined))
	if in := <-p.i.Inbound(); in.Type != wire.TypeImgReject {
		t.Fatal(in.Type)
	}
	// Stream closed on both sides; a late cancel from I must be ignored by R.
	p.i.streams.mu.Lock()
	p.i.closedQueues[id2] = make(chan outFrame, 1)
	p.i.closedQueues[id2] <- outFrame{typ: wire.TypeImgCancel, stream: id2, payload: wire.AppendImgCancel(nil, 0)}
	p.i.streams.mu.Unlock()
	p.i.transferWake <- struct{}{}
	time.Sleep(50 * time.Millisecond)
	if p.r.Err() != nil {
		t.Fatal("late cancel closed the session")
	}
}

// --- adversarial rows (§13.3, image cases) via the raw test peer ---

func offerPayload(size uint64) []byte {
	p, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: size, Format: 1, Width: 1, Height: 1})
	return p
}

func TestImageViolations(t *testing.T) {
	type tc struct {
		name string
		run  func(t *testing.T, a *adv)
	}
	cases := []tc{
		{"wrong parity stream id", func(t *testing.T, a *adv) {
			_ = a.p.SendFrame(wire.TypeImgOffer, 3, offerPayload(1)) // peer (responder role) must use odd; session is responder, peer is initiator → even expected
		}},
		{"reused stream id", func(t *testing.T, a *adv) {
			_ = a.p.SendFrame(wire.TypeImgOffer, 2, offerPayload(1))
			a.recvType(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgOffer, 2, offerPayload(1))
		}},
		{"chunk before accept", func(t *testing.T, a *adv) {
			_ = a.p.SendFrame(wire.TypeImgOffer, 2, offerPayload(1))
			a.recvType(t, wire.TypeImgOffer)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: []byte{1}})
			_ = a.p.SendFrame(wire.TypeImgChunk, 2, c)
		}},
		{"chunk index out of order", func(t *testing.T, a *adv) {
			a.acceptOffer(t, 2, 2*wire.ChunkData)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 1, Data: make([]byte, wire.ChunkData)})
			_ = a.p.SendFrame(wire.TypeImgChunk, 2, c)
		}},
		{"wrong chunk size", func(t *testing.T, a *adv) {
			a.acceptOffer(t, 2, 2*wire.ChunkData)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: make([]byte, wire.ChunkData-1)})
			_ = a.p.SendFrame(wire.TypeImgChunk, 2, c)
		}},
		{"wrong last chunk size", func(t *testing.T, a *adv) {
			a.acceptOffer(t, 2, wire.ChunkData+10)
			c, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: make([]byte, wire.ChunkData)})
			_ = a.p.SendFrame(wire.TypeImgChunk, 2, c)
			c, _ = wire.AppendImgChunk(nil, wire.ImgChunk{Index: 1, Data: make([]byte, 11)})
			_ = a.p.SendFrame(wire.TypeImgChunk, 2, c)
		}},
		{"done before all chunks", func(t *testing.T, a *adv) {
			a.acceptOffer(t, 2, 2*wire.ChunkData)
			_ = a.p.SendFrame(wire.TypeImgDone, 2, nil)
		}},
		{"unknown cancel reason", func(t *testing.T, a *adv) {
			a.acceptOffer(t, 2, 10)
			_ = a.p.SendFrame(wire.TypeImgCancel, 2, []byte{7})
		}},
		{"frame on closed id", func(t *testing.T, a *adv) {
			_ = a.p.SendFrame(wire.TypeImgOffer, 2, offerPayload(1))
			a.recvType(t, wire.TypeImgOffer)
			_ = a.s.SendStream(context.Background(), 2, wire.TypeImgReject, wire.AppendImgReject(nil, 0))
			if in := a.recv(t, wire.TypeImgReject); in.Stream != 2 {
				t.Fatal("reject")
			}
			_ = a.p.SendFrame(wire.TypeImgAccept, 2, wire.AppendImgAccept(nil, 0))
		}},
		{"offer churn 17 in 60s", func(t *testing.T, a *adv) {
			for i := 0; i <= wire.MaxOffersPerWindow; i++ {
				id := uint16(2 + 2*i)
				_ = a.p.SendFrame(wire.TypeImgOffer, id, offerPayload(1))
				if i < wire.MaxPendingOffers {
					a.recvType(t, wire.TypeImgOffer)
				} else if i < wire.MaxOffersPerWindow {
					a.recv(t, wire.TypeImgReject) // busy
				}
			}
		}},
		{"accept start_chunk beyond end", func(t *testing.T, a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgAccept, id, wire.AppendImgAccept(nil, 1))
		}},
		{"result before done", func(t *testing.T, a *adv) {
			id, _ := a.s.OpenStream(10)
			_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(10))
			a.recv(t, wire.TypeImgOffer)
			_ = a.p.SendFrame(wire.TypeImgResult, id, wire.AppendImgResult(nil, 0))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newAdv(t, false, true) // session is responder; the raw peer (initiator) offers even ids
			c.run(t, a)
			a.expectViolation(t)
		})
	}
}

// recvType drains the session's Inbound until a frame of the given type.
func (a *adv) recvType(t *testing.T, typ wire.FrameType) Inbound {
	t.Helper()
	select {
	case in := <-a.s.Inbound():
		if in.Type != typ {
			t.Fatalf("inbound %#x want %#x", in.Type, typ)
		}
		return in
	case <-time.After(3 * time.Second):
		t.Fatal("no inbound frame")
	}
	return Inbound{}
}

// acceptOffer makes the raw peer offer on stream id and the session accept it.
func (a *adv) acceptOffer(t *testing.T, id uint16, size uint64) {
	t.Helper()
	if err := a.p.SendFrame(wire.TypeImgOffer, id, offerPayload(size)); err != nil {
		t.Fatal(err)
	}
	a.recvType(t, wire.TypeImgOffer)
	if err := a.s.SendStream(context.Background(), id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); err != nil {
		t.Fatal(err)
	}
	a.recv(t, wire.TypeImgAccept)
}

func TestBusyRejectAndDrainingLimits(t *testing.T) {
	// 5th pending offer → IMG_REJECT busy without an inbound event.
	a := newAdv(t, false, true)
	for i := 0; i < wire.MaxPendingOffers; i++ {
		_ = a.p.SendFrame(wire.TypeImgOffer, uint16(2+2*i), offerPayload(1))
		a.recvType(t, wire.TypeImgOffer)
	}
	_ = a.p.SendFrame(wire.TypeImgOffer, uint16(2+2*wire.MaxPendingOffers), offerPayload(1))
	in := a.recv(t, wire.TypeImgReject)
	if r, _ := wire.DecodeImgReject(in.Payload); r != wire.RejectBusy {
		t.Fatal(r)
	}
	select {
	case in := <-a.s.Inbound():
		t.Fatalf("busy offer delivered: %#x", in.Type)
	case <-time.After(50 * time.Millisecond):
	}
	if a.s.Err() != nil {
		t.Fatal(a.s.Err())
	}

	// 17 draining streams → limit: we offer, then cancel; the raw peer never
	// answers, so every cancelled stream stays in draining.
	b := newAdv(t, true, true) // session initiator → our own even ids, no offer churn
	for i := 0; i < wire.MaxDrainingStreams+1; i++ {
		id, err := b.s.OpenStream(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(1)); err != nil {
			t.Fatal(err)
		}
		b.recv(t, wire.TypeImgOffer)
		err = b.s.SendStream(context.Background(), id, wire.TypeImgCancel, wire.AppendImgCancel(nil, 0))
		if i == wire.MaxDrainingStreams {
			if !errors.Is(err, ErrStreamLimit) {
				t.Fatalf("17th draining stream accepted: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(i, err)
		}
		b.recv(t, wire.TypeImgCancel)
	}
}

// A raw receiver that never accepts sees the offer; the session sender closes
// the stream on IMG_REJECT and can reuse the slot (ids never reused).
func TestSenderRejectPath(t *testing.T) {
	a := newAdv(t, true, true) // session initiator → even ids
	id, _ := a.s.OpenStream(1)
	_ = a.s.SendStream(context.Background(), id, wire.TypeImgOffer, offerPayload(1))
	a.recv(t, wire.TypeImgOffer)
	_ = a.p.SendFrame(wire.TypeImgReject, id, wire.AppendImgReject(nil, wire.RejectTooLarge))
	in := a.recvType(t, wire.TypeImgReject)
	if r, _ := wire.DecodeImgReject(in.Payload); r != wire.RejectTooLarge || a.s.StreamOpen(id) {
		t.Fatal("reject not applied")
	}
	id2, _ := a.s.OpenStream(1)
	if id2 != id+2 {
		t.Fatal("id reused")
	}
	_ = a.p.SendFrame(wire.TypeImgOffer, 3, offerPayload(1)) // peer (responder) offers odd: fine
	a.recvType(t, wire.TypeImgOffer)
	if a.s.Err() != nil {
		t.Fatal(a.s.Err())
	}
}

func BenchmarkTransfer(b *testing.B) {
	// End-to-end throughput of the session layer over net.Pipe (no disk, no TCP).
	t := &testing.T{}
	p := newPair(t, time.Hour)
	const size = 256 * wire.ChunkData // exactly 256 full chunks
	data := make([]byte, wire.ChunkData)
	b.SetBytes(size)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ctx := context.Background()
		// A real peer may offer at most 16 images per minute; the benchmark
		// resets the receiver's churn window so b.N iterations are not a violation.
		p.r.streams.mu.Lock()
		p.r.streams.offerTimes = nil
		p.r.streams.mu.Unlock()
		id, _ := p.i.OpenStream(size)
		op, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: size, Format: 1})
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-p.r.Inbound()
			_ = p.r.SendStream(ctx, id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0))
			for {
				in := <-p.r.Inbound()
				if in.Type == wire.TypeImgDone {
					break
				}
			}
			_ = p.r.SendStream(ctx, id, wire.TypeImgResult, wire.AppendImgResult(nil, 0))
		}()
		_ = p.i.SendStream(ctx, id, wire.TypeImgOffer, op)
		<-p.i.Inbound()
		for i := 0; i < size/wire.ChunkData; i++ {
			cp, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: uint32(i), Data: data})
			_ = p.i.SendStream(ctx, id, wire.TypeImgChunk, cp)
		}
		_ = p.i.SendStream(ctx, id, wire.TypeImgDone, nil)
		<-p.i.Inbound()
		<-done
	}
	p.cancel()
}

// acceptOfferWithSink is acceptOffer with chunk data routed to sink.
func (a *adv) acceptOfferWithSink(t *testing.T, id uint16, size uint64, sink chan<- []byte) {
	t.Helper()
	if err := a.p.SendFrame(wire.TypeImgOffer, id, offerPayload(size)); err != nil {
		t.Fatal(err)
	}
	a.recvType(t, wire.TypeImgOffer)
	if err := a.s.SetChunkSink(id, sink); err != nil {
		t.Fatal(err)
	}
	if err := a.s.SendStream(context.Background(), id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); err != nil {
		t.Fatal(err)
	}
	a.recv(t, wire.TypeImgAccept)
}

// A file offer opens a stream exactly like an image offer and the rest of the
// transfer uses the same frames.
func TestFileOfferOpensStream(t *testing.T) {
	p := newPair(t, time.Hour)
	ctx := context.Background()
	data := []byte("PK\x03\x04 not really a zip")
	id, err := p.i.OpenStream(uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := wire.AppendFileOffer(nil, wire.FileOffer{Size: uint64(len(data)), Hash: blake2b.Sum256(data), Ext: []byte("zip")})
	if err := p.i.SendStream(ctx, id, wire.TypeFileOffer, op); err != nil {
		t.Fatal(err)
	}
	in := <-p.r.Inbound()
	o, err := wire.DecodeFileOffer(in.Payload)
	if in.Type != wire.TypeFileOffer || err != nil || string(o.Ext) != "zip" {
		t.Fatalf("%#x %v", in.Type, err)
	}
	if err := p.r.SendStream(ctx, id, wire.TypeImgAccept, wire.AppendImgAccept(nil, 0)); err != nil {
		t.Fatal(err)
	}
	if in = <-p.i.Inbound(); in.Type != wire.TypeImgAccept {
		t.Fatalf("%#x", in.Type)
	}
	cp, _ := wire.AppendImgChunk(nil, wire.ImgChunk{Index: 0, Data: data})
	if err := p.i.SendStream(ctx, id, wire.TypeImgChunk, cp); err != nil {
		t.Fatal(err)
	}
	if in = <-p.r.Inbound(); in.Type != wire.TypeImgChunk {
		t.Fatalf("%#x", in.Type)
	}
	// A second offer on the same id is a violation, whatever its kind.
	if err := p.i.SendStream(ctx, id, wire.TypeFileOffer, op); !errors.Is(err, ErrStreamState) {
		t.Fatal(err)
	}
}

// An offer of a kind we did not advertise, or one that does not decode, is a
// protocol violation.
func TestOfferNeedsAdvertisedFeature(t *testing.T) {
	file, _ := wire.AppendFileOffer(nil, wire.FileOffer{Size: 1, Ext: []byte("pdf")})
	img, _ := wire.AppendImgOffer(nil, wire.ImgOffer{Size: 1, Format: wire.FormatPNG, Width: 1, Height: 1})
	for _, c := range []struct {
		name string
		typ  wire.FrameType
		pay  []byte
		ours uint64
		ok   bool
	}{
		{"file advertised", wire.TypeFileOffer, file, wire.FeatureFiles, true},
		{"file not advertised", wire.TypeFileOffer, file, wire.FeatureImages, false},
		{"image advertised", wire.TypeImgOffer, img, wire.FeatureImages, true},
		{"image not advertised", wire.TypeImgOffer, img, wire.FeatureFiles, false},
		{"image payload in a file offer", wire.TypeFileOffer, img, wire.FeatureFiles, false},
		{"empty", wire.TypeFileOffer, nil, wire.FeatureFiles, false},
	} {
		size, err := offerSize(wire.Inner{Type: c.typ, Stream: 2, Payload: c.pay}, c.ours)
		if c.ok != (err == nil) || (c.ok && size != 1) {
			t.Errorf("%s: size %d err %v", c.name, size, err)
		}
	}
}

func TestBadFileOfferClosesSession(t *testing.T) {
	a := newAdv(t, true, true)
	bad, _ := wire.AppendFileOffer(nil, wire.FileOffer{Size: 1, Ext: []byte("zip")})
	bad[8+wire.HashSize+1] = '/' // path separator inside the extension
	if err := a.p.SendFrame(wire.TypeFileOffer, 3, bad); err != nil {
		t.Fatal(err)
	}
	a.expectViolation(t)
}
