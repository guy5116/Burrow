package session

import (
	"bufio"
	"fmt"

	"github.com/guy5116/burrow/internal/buf"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

const writerBufSize = 256 << 10

type writer = *bufio.Writer

// writeLoop owns the SEND chain and the outbound queues. Frames are encrypted
// at the instant they are written, with the SEND chain at that instant.
func (s *Session) writeLoop() {
	w := bufio.NewWriterSize(s.conn, writerBufSize)
	fb := buf.Get()
	defer buf.Put(fb)

	// Our HELLO is always our first frame; the responder waits for the peer's.
	if !s.cfg.Initiator {
		select {
		case <-s.helloRecv:
		case <-s.ctx.Done():
			return
		}
	}
	hello, err := wire.AppendHello(nil, s.cfg.Hello)
	if err != nil {
		s.fail(err)
		return
	}
	if err := s.writeFrame(w, *fb, wire.TypeHello, wire.StreamControl, hello); err != nil {
		s.fail(err)
		return
	}
	// Nothing else may be sent before the peer's HELLO validated.
	select {
	case <-s.helloRecv:
	case <-s.ctx.Done():
		return
	}
	s.ready.Store(true)
	close(s.readyCh)

	for {
		// Strict priority: control > chat.
		select {
		case c := <-s.control:
			if err := s.handleCtrl(w, *fb, c); err != nil {
				s.fail(err)
				return
			}
			continue
		default:
		}
		select {
		case f := <-s.chat:
			if err := s.writeFrame(w, *fb, f.typ, f.stream, f.payload); err != nil {
				s.fail(err)
				return
			}
			continue
		default:
		}
		if f, ok := s.nextTransferFrame(); ok {
			err := s.writeTransfer(w, *fb, f)
			buf.Put(f.pooled)
			if err != nil {
				s.fail(err)
				return
			}
			continue
		}
		if err := w.Flush(); err != nil { // queues drained: push out buffered chunks
			s.fail(err)
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case c := <-s.control:
			if err := s.handleCtrl(w, *fb, c); err != nil {
				s.fail(err)
				return
			}
		case f := <-s.chat:
			if err := s.writeFrame(w, *fb, f.typ, f.stream, f.payload); err != nil {
				s.fail(err)
				return
			}
		case <-s.transferWake:
		}
	}
}

// writeTransfer encrypts and buffers a transfer frame; it is flushed when the
// queues drain or the 256 KiB buffer fills, so chat never waits behind more
// than one chunk. Frames on closed streams (echoes, busy rejects) are dropped
// from the closed-queue map once written.
func (s *Session) writeTransfer(w *bufio.Writer, fb []byte, f outFrame) error {
	inner, err := wire.EncodeInner(fb[wire.OuterLenSize:wire.OuterLenSize], f.typ, f.stream, f.payload)
	if err != nil {
		return err
	}
	frame, err := s.seal(s.send, fb, len(inner))
	if err != nil {
		return err
	}
	s.sendCounter.Store(s.send.counter)
	if err := s.conn.SetWriteDeadline(s.now().Add(wire.WriteDeadline)); err != nil {
		return err
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	s.lastWrittenAt.Store(s.now().UnixNano())
	clear(fb[:len(frame)])
	s.streams.mu.Lock()
	if q, ok := s.closedQueues[f.stream]; ok && len(q) == 0 {
		delete(s.closedQueues, f.stream)
	}
	s.streams.mu.Unlock()
	return nil
}

// writeFrame encodes, encrypts under the current SEND chain, writes and
// flushes (every stream-0/1 frame is flushed immediately, §11).
func (s *Session) writeFrame(w *bufio.Writer, fb []byte, typ wire.FrameType, stream uint16, payload []byte) error {
	inner, err := wire.EncodeInner(fb[wire.OuterLenSize:wire.OuterLenSize], typ, stream, payload)
	if err != nil {
		return err
	}
	frame, err := s.seal(s.send, fb, len(inner))
	if err != nil {
		return err
	}
	s.sendCounter.Store(s.send.counter)
	if err := s.conn.SetWriteDeadline(s.now().Add(wire.WriteDeadline)); err != nil {
		return err
	}
	if _, err := w.Write(frame); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	s.lastWrittenAt.Store(s.now().UnixNano())
	clear(fb[:len(frame)])
	return nil
}

func (s *Session) handleCtrl(w *bufio.Writer, fb []byte, c ctrl) error {
	switch c.kind {
	case ctrlStartRekey:
		if !s.cfg.Initiator || s.rekeyInFlight.Load() {
			return nil // discarded: not our role, or already in flight (harmless race, §3.5)
		}
		return s.startRekey(w, fb)
	case ctrlSendPing:
		if s.pingNonce.Load() != s.pongNonce.Load() {
			return nil // discarded: a PING is outstanding (§4.5)
		}
		nonce, err := randomNonce()
		if err != nil {
			return err
		}
		s.pingNonce.Store(nonce)
		return s.writeFrame(w, fb, wire.TypePing, wire.StreamControl, wire.AppendPing(nil, nonce))
	case ctrlSendRekeyRequest:
		if s.cfg.Initiator || s.requestSentAt.Load() != 0 {
			return nil
		}
		if err := s.writeFrame(w, fb, wire.TypeRekeyRequest, wire.StreamControl, nil); err != nil {
			return err
		}
		s.requestSentAt.Store(s.now().UnixNano())
		return nil
	case ctrlPong:
		return s.writeFrame(w, fb, wire.TypePong, wire.StreamControl, c.payload)
	case ctrlRekeyResp:
		if err := s.writeFrame(w, fb, wire.TypeRekeyResp, wire.StreamControl, c.payload); err != nil {
			c.chain.Clear()
			return err
		}
		s.switchSend(c.chain)
		s.requestSentAt.Store(0)
		return nil
	case ctrlRekeyDone:
		if err := s.writeFrame(w, fb, wire.TypeRekeyDone, wire.StreamControl, nil); err != nil {
			c.chain.Clear()
			return err
		}
		s.switchSend(c.chain)
		s.rekeyInFlight.Store(false)
		return nil
	case ctrlBye:
		if err := s.writeFrame(w, fb, wire.TypeBye, wire.StreamControl, wire.AppendBye(nil, c.reason)); err != nil {
			return err
		}
		return ErrClosed
	}
	return fmt.Errorf("session: unknown control %d", c.kind)
}

// switchSend installs the next epoch's SEND chain and clears the old one.
func (s *Session) switchSend(ck *secret.Buffer) {
	old := s.send
	s.send = newChain(ck, old.epoch+1)
	old.clear()
	s.sendEpoch.Store(s.send.epoch)
	s.sendCounter.Store(0)
	s.sendSwitchedAt.Store(s.now().UnixNano())
}
