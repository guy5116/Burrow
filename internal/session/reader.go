package session

import (
	"io"
	"time"

	"github.com/guy5116/burrow/internal/buf"
	"github.com/guy5116/burrow/internal/wire"
)

// readLoop owns the RECV chain, root_n and all rekey derivation.
func (s *Session) readLoop() {
	fb := buf.Get()
	defer buf.Put(fb)
	helloSeen := false
	for {
		if err := s.conn.SetReadDeadline(s.now().Add(wire.DeadPeerTimeout)); err != nil {
			s.fail(err)
			return
		}
		hdr := (*fb)[:wire.OuterLenSize]
		if _, err := io.ReadFull(s.conn, hdr); err != nil {
			s.fail(err)
			return
		}
		n, err := wire.OuterLen(hdr)
		if err != nil {
			s.fail(ErrProtocol)
			return
		}
		body := (*fb)[wire.OuterLenSize : wire.OuterLenSize+n]
		if _, err := io.ReadFull(s.conn, body); err != nil {
			s.fail(err)
			return
		}
		pt, err := s.open(s.recv, body)
		if err != nil {
			s.fail(err)
			return
		}
		s.recvCounter.Store(s.recv.counter)
		in, err := wire.DecodeInner(pt)
		if err != nil {
			s.fail(ErrProtocol)
			return
		}
		if !helloSeen {
			if in.Type != wire.TypeHello {
				s.fail(ErrProtocol)
				return
			}
			h, err := wire.DecodeHello(in.Payload)
			if err != nil {
				s.fail(ErrProtocol)
				return
			}
			s.peerHello = h
			s.peerHello.Name = append([]byte(nil), h.Name...)
			helloSeen = true
			close(s.helloRecv)
		} else if err := s.dispatch(in); err != nil {
			s.fail(err)
			return
		}
		clear(pt)
	}
}

// dispatch handles one post-HELLO frame. Any illegal frame → ErrProtocol.
func (s *Session) dispatch(in wire.Inner) error {
	switch in.Type {
	case wire.TypeHello:
		return ErrProtocol // exactly once
	case wire.TypePing:
		nonce, err := wire.DecodePing(in.Payload)
		if err != nil {
			return ErrProtocol
		}
		now := s.now()
		s.pingTimes = trimWindow(s.pingTimes, now, wire.PingFloodWindow)
		s.pingTimes = append(s.pingTimes, now)
		if len(s.pingTimes) > wire.MaxPingsPerWindow {
			return ErrProtocol
		}
		return s.pushControlCtx(ctrl{kind: ctrlPong, payload: wire.AppendPong(nil, nonce)})
	case wire.TypePong:
		nonce, err := wire.DecodePong(in.Payload)
		if err != nil {
			return ErrProtocol
		}
		pn := s.pingNonce.Load()
		if nonce != pn || pn == s.pongNonce.Load() {
			return ErrProtocol
		}
		s.pongNonce.Store(nonce)
		return nil
	case wire.TypeBye:
		reason, err := wire.DecodeBye(in.Payload)
		if err != nil {
			return ErrProtocol
		}
		return &ByeError{Reason: reason}
	case wire.TypeRekeyInit:
		return s.onRekeyInit(in.Payload)
	case wire.TypeRekeyResp:
		return s.onRekeyResp(in.Payload)
	case wire.TypeRekeyDone:
		return s.onRekeyDone(in.Payload)
	case wire.TypeRekeyRequest:
		if wire.DecodeEmpty(in.Payload) != nil || !s.cfg.Initiator {
			return ErrProtocol
		}
		return s.pushControlCtx(ctrl{kind: ctrlStartRekey})
	case wire.TypeText:
		if _, err := wire.DecodeText(in.Payload); err != nil {
			return ErrProtocol
		}
	case wire.TypeAck:
		if _, err := wire.DecodeAck(in.Payload); err != nil {
			return ErrProtocol
		}
	case wire.TypeTyping:
		if _, err := wire.DecodeTyping(in.Payload); err != nil {
			return ErrProtocol
		}
		now := s.now()
		s.typingTimes = trimWindow(s.typingTimes, now, wire.TypingWindow)
		if len(s.typingTimes) >= wire.MaxTypingPerWindow {
			return nil // drop, do not close
		}
		s.typingTimes = append(s.typingTimes, now)
	case wire.TypeImgOffer, wire.TypeImgAccept, wire.TypeImgReject, wire.TypeImgChunk, wire.TypeImgDone, wire.TypeImgResult, wire.TypeImgCancel:
		return s.onStreamFrame(in)
	default:
		return ErrProtocol
	}
	return s.deliver(Inbound{Type: in.Type, Stream: in.Stream, Payload: append([]byte(nil), in.Payload...)})
}

// pushControlCtx enqueues a reply the reader must send, waiting up to
// ControlQueueStall for room (a full queue for that long means the writer is stalled).
func (s *Session) pushControlCtx(c ctrl) error {
	select {
	case s.control <- c:
		return nil
	default:
	}
	t := time.NewTimer(wire.ControlQueueStall)
	defer t.Stop()
	select {
	case s.control <- c:
		return nil
	case <-t.C:
		c.chain.Clear()
		return ErrProtocol
	case <-s.ctx.Done():
		c.chain.Clear()
		return ErrClosed
	}
}

func trimWindow(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	return append(ts[:0], ts[i:]...)
}
