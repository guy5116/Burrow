package session

// Helpers that only the tests of this package need.

import (
	"context"

	"github.com/guy5116/burrow/internal/wire"
)

// Send queues a chat frame (TEXT, ACK, TYPING). It blocks under backpressure
// until ctx or the session ends. payload is copied.
func (s *Session) Send(ctx context.Context, typ wire.FrameType, payload []byte) error {
	if typ.Class() != wire.ClassChat {
		return wire.ErrStream
	}
	if !s.ready.Load() {
		return ErrNotReady
	}
	select {
	case <-s.closed:
		return ErrClosed
	default:
	}
	f := outFrame{typ: typ, stream: wire.StreamChat, payload: append([]byte(nil), payload...)}
	select {
	case s.chat <- f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.closed:
		return ErrClosed
	}
}

// Rekey asks the writer to start a rekey now (initiator) or to send
// REKEY_REQUEST (responder).
func (s *Session) Rekey() error {
	kind := ctrlStartRekey
	if !s.cfg.Initiator {
		kind = ctrlSendRekeyRequest
	}
	return s.pushControl(ctrl{kind: kind})
}

// Epochs reports the current send and receive epochs.
func (s *Session) Epochs() (send, recv uint32) {
	return uint32(s.sendEpoch.Load()), uint32(s.recvEpoch.Load())
}

// StreamOpen reports whether id is still open.
func (s *Session) StreamOpen(id uint16) bool {
	s.streams.mu.Lock()
	defer s.streams.mu.Unlock()
	_, ok := s.streams.open[id]
	return ok
}
