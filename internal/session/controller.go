package session

import (
	"crypto/rand"
	"encoding/binary"
	"time"

	"github.com/guy5116/burrow/internal/wire"
)

// controlLoop owns timers only: rekey schedule, PING schedule, REQUEST
// timeout and the time-based fail-safes. It never touches key material.
func (s *Session) controlLoop() {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	var sched schedule
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		if !s.ready.Load() {
			continue
		}
		now := s.now()
		if err := s.tick(now, &sched); err != nil {
			s.fail(err)
			return
		}
	}
}

// schedule is the controller's memory between ticks. Each command is pushed
// once per occasion: while the writer is busy the condition that asked for it
// stays true, and pushing again every tick would fill the control queue.
type schedule struct {
	fullSince   time.Time // when the control queue was first found full
	rekeyFor    int64     // sendSwitchedAt of the epoch a rekey or request was pushed for
	pingAfter   uint64    // pingNonce when the last SendPing was pushed
	pingPending bool
}

// tick evaluates the schedule once.
func (s *Session) tick(now time.Time, sched *schedule) error {
	since := func(nanos int64) time.Duration { return now.Sub(time.Unix(0, nanos)) }
	sc, rc := s.sendCounter.Load(), s.recvCounter.Load()
	epoch := s.sendSwitchedAt.Load()

	// Fail-safes.
	if since(epoch) > wire.RekeyEpochFailSafe || since(s.recvSwitchedAt.Load()) > wire.RekeyEpochFailSafe {
		return ErrProtocol
	}
	if s.rekeyInFlight.Load() && since(s.rekeyStartedAt.Load()) > wire.RekeyInFlightTimeout {
		return ErrProtocol
	}
	if s.readerRekeyState.Load() == rekeyAwaitingDone && since(s.readerStateSince.Load()) > wire.RekeyInFlightTimeout {
		return ErrProtocol
	}
	if rs := s.requestSentAt.Load(); rs != 0 && since(rs) > wire.RekeyRequestTimeout {
		return ErrProtocol
	}

	var cmds []ctrl
	due := func(counter uint64, interval time.Duration) bool {
		return sched.rekeyFor != epoch && (sc >= counter || rc >= counter || since(epoch) >= interval)
	}
	if s.cfg.Initiator {
		if !s.rekeyInFlight.Load() && due(wire.RekeyInitCounter, wire.RekeyInitInterval) {
			cmds = append(cmds, ctrl{kind: ctrlStartRekey})
		}
	} else if s.readerRekeyState.Load() == rekeyIdle && s.requestSentAt.Load() == 0 && due(wire.RekeyRequestCounter, wire.RekeyRequestInterval) {
		cmds = append(cmds, ctrl{kind: ctrlSendRekeyRequest})
	}
	ping := s.pingNonce.Load()
	if sched.pingPending && ping != sched.pingAfter {
		sched.pingPending = false // the writer has sent it
	}
	if !sched.pingPending && since(s.lastWrittenAt.Load()) >= wire.PingInterval && ping == s.pongNonce.Load() {
		cmds = append(cmds, ctrl{kind: ctrlSendPing})
	}
	for _, c := range cmds {
		if err := s.pushControl(c); err != nil {
			if sched.fullSince.IsZero() {
				sched.fullSince = now
			} else if now.Sub(sched.fullSince) >= wire.ControlQueueStall {
				return ErrProtocol // writer stalled
			}
			return nil
		}
		if c.kind == ctrlSendPing {
			sched.pingPending, sched.pingAfter = true, ping
		} else {
			sched.rekeyFor = epoch
		}
	}
	sched.fullSince = time.Time{}
	return nil
}

// randomNonce draws a non-zero PING nonce from crypto/rand.
func randomNonce() (uint64, error) {
	for {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return 0, err
		}
		if n := binary.BigEndian.Uint64(b[:]); n != 0 {
			return n, nil
		}
	}
}
