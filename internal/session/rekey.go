package session

import (
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/sha256"

	"golang.org/x/crypto/curve25519"

	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

// startRekey (writer, initiator): generate e_i' and kem_seed', hand them to the
// reader through `pending`, and write REKEY_INIT under the current epoch.
func (s *Session) startRekey(w writer, fb []byte) error {
	esc, err := secret.Random(wire.X25519Size)
	if err != nil {
		return err
	}
	epub, err := curve25519.X25519(esc.Bytes(), curve25519.Basepoint)
	if err != nil {
		esc.Clear()
		return err
	}
	seed, err := secret.Random(wire.KEMSeedSize)
	if err != nil {
		esc.Clear()
		return err
	}
	dk, err := mlkem.NewDecapsulationKey768(seed.Bytes())
	if err != nil {
		esc.Clear()
		seed.Clear()
		return err
	}
	var init wire.RekeyInit
	copy(init.EPub[:], epub)
	copy(init.KemEK[:], dk.EncapsulationKey().Bytes())
	p := &pendingRekey{escalar: esc, kemSeed: seed}
	select {
	case s.pending <- p:
	default:
		p.clear()
		return ErrProtocol // cap-1 channel full: a bug, close
	}
	s.rekeyInFlight.Store(true)
	s.rekeyStartedAt.Store(s.now().UnixNano())
	return s.writeFrame(w, fb, wire.TypeRekeyInit, wire.StreamControl, wire.AppendRekeyInit(nil, &init))
}

// onRekeyInit (reader, responder).
func (s *Session) onRekeyInit(payload []byte) error {
	if s.cfg.Initiator || s.readerRekeyState.Load() != rekeyIdle {
		return ErrProtocol
	}
	var init wire.RekeyInit
	if err := wire.DecodeRekeyInit(payload, &init); err != nil {
		return ErrProtocol
	}
	ek, err := mlkem.NewEncapsulationKey768(init.KemEK[:])
	if err != nil {
		return ErrProtocol
	}
	esc, err := secret.Random(wire.X25519Size)
	if err != nil {
		return err
	}
	defer esc.Clear()
	epub, err := curve25519.X25519(esc.Bytes(), curve25519.Basepoint)
	if err != nil {
		return err
	}
	kemSS, kemCT := ek.Encapsulate()
	ss := secret.From(kemSS)
	defer ss.Clear()
	// R computes: dh_ee = X25519(e_r', e_i'.pub)  dh_es = X25519(s_r, e_i'.pub)  dh_se = X25519(e_r', s_i.pub)
	peer := s.cfg.Peer
	i2r, r2i, err := s.deriveNext(esc.Bytes(), init.EPub[:], s.cfg.Self.Scalar(), init.EPub[:], esc.Bytes(), peer[:], ss)
	if err != nil {
		return err
	}
	s.nextRecv = newChain(i2r, s.recv.epoch+1)
	s.readerRekeyState.Store(rekeyAwaitingDone)
	s.readerStateSince.Store(s.now().UnixNano())
	var resp wire.RekeyResp
	copy(resp.EPub[:], epub)
	copy(resp.KemCT[:], kemCT)
	return s.pushControlCtx(ctrl{kind: ctrlRekeyResp, payload: wire.AppendRekeyResp(nil, &resp), chain: r2i})
}

// onRekeyResp (reader, initiator).
func (s *Session) onRekeyResp(payload []byte) error {
	if !s.cfg.Initiator || !s.rekeyInFlight.Load() {
		return ErrProtocol
	}
	var p *pendingRekey
	select {
	case p = <-s.pending:
	default:
		return ErrProtocol
	}
	defer p.clear()
	var resp wire.RekeyResp
	if err := wire.DecodeRekeyResp(payload, &resp); err != nil {
		return ErrProtocol
	}
	dk, err := mlkem.NewDecapsulationKey768(p.kemSeed.Bytes())
	if err != nil {
		return err
	}
	kemSS, err := dk.Decapsulate(resp.KemCT[:])
	if err != nil {
		return ErrProtocol
	}
	ss := secret.From(kemSS)
	defer ss.Clear()
	// I computes: dh_ee = X25519(e_i', e_r'.pub)  dh_es = X25519(e_i', s_r.pub)  dh_se = X25519(s_i, e_r'.pub)
	peer := s.cfg.Peer
	i2r, r2i, err := s.deriveNext(p.escalar.Bytes(), resp.EPub[:], p.escalar.Bytes(), peer[:], s.cfg.Self.Scalar(), resp.EPub[:], ss)
	if err != nil {
		return err
	}
	s.switchRecv(newChain(r2i, s.recv.epoch+1))
	return s.pushControlCtx(ctrl{kind: ctrlRekeyDone, chain: i2r})
}

// onRekeyDone (reader, responder).
func (s *Session) onRekeyDone(payload []byte) error {
	if s.cfg.Initiator || wire.DecodeEmpty(payload) != nil || s.readerRekeyState.Load() != rekeyAwaitingDone {
		return ErrProtocol
	}
	s.switchRecv(s.nextRecv)
	s.nextRecv = nil
	s.readerRekeyState.Store(rekeyIdle)
	return nil
}

func (s *Session) switchRecv(c *chain) {
	old := s.recv
	s.recv = c
	old.clear()
	s.recvEpoch.Store(c.epoch)
	s.recvCounter.Store(0)
	s.recvSwitchedAt.Store(s.now().UnixNano())
}

// deriveNext computes root_{n+1} = HKDF-Extract(dh_ee || dh_es || dh_se || kem_ss, salt = root_n)
// and the next epoch's chains, replacing root_n. Arguments are (scalar, point) pairs.
func (s *Session) deriveNext(eeS, eeP, esS, esP, seS, seP []byte, ss *secret.Buffer) (i2r, r2i *secret.Buffer, err error) {
	ikm := secret.New(4 * 32)
	defer ikm.Clear()
	b := ikm.Bytes()
	for i, pair := range [][2][]byte{{eeS, eeP}, {esS, esP}, {seS, seP}} {
		dh, err := curve25519.X25519(pair[0], pair[1]) // all-zero result → error → close
		if err != nil {
			return nil, nil, ErrProtocol
		}
		copy(b[i*32:], dh)
		secret.Wipe(dh)
	}
	copy(b[96:], ss.Bytes())
	prk, err := hkdf.Extract(sha256.New, b, s.root.Bytes())
	if err != nil {
		return nil, nil, err
	}
	s.root.Clear()
	s.root = secret.From(prk)
	return deriveChains(s.root, s.recv.epoch+1)
}
