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
	var fullSince time.Time
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
		if err := s.tick(now, &fullSince); err != nil {
			s.fail(err)
			return
		}
	}
}

// tick evaluates the schedule once (exported to tests through Tick()).
func (s *Session) tick(now time.Time, fullSince *time.Time) error {
	since := func(nanos int64) time.Duration { return now.Sub(time.Unix(0, nanos)) }
	sc, rc := s.sendCounter.Load(), s.recvCounter.Load()

	// Fail-safes.
	if since(s.sendSwitchedAt.Load()) > wire.RekeyEpochFailSafe || since(s.recvSwitchedAt.Load()) > wire.RekeyEpochFailSafe {
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
	if s.cfg.Initiator {
		if !s.rekeyInFlight.Load() && (sc >= wire.RekeyInitCounter || rc >= wire.RekeyInitCounter || since(s.sendSwitchedAt.Load()) >= wire.RekeyInitInterval) {
			cmds = append(cmds, ctrl{kind: ctrlStartRekey})
		}
	} else if s.readerRekeyState.Load() == rekeyIdle && s.requestSentAt.Load() == 0 &&
		(sc >= wire.RekeyRequestCounter || rc >= wire.RekeyRequestCounter || since(s.sendSwitchedAt.Load()) >= wire.RekeyRequestInterval) {
		cmds = append(cmds, ctrl{kind: ctrlSendRekeyRequest})
	}
	if since(s.lastWrittenAt.Load()) >= wire.PingInterval && s.pingNonce.Load() == s.pongNonce.Load() {
		cmds = append(cmds, ctrl{kind: ctrlSendPing})
	}
	for _, c := range cmds {
		if err := s.pushControl(c); err != nil {
			if fullSince.IsZero() {
				*fullSince = now
			} else if now.Sub(*fullSince) >= wire.ControlQueueStall {
				return ErrProtocol // writer stalled
			}
			return nil
		}
	}
	*fullSince = time.Time{}
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
