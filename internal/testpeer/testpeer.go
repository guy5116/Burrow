// Package testpeer is a scriptable, deliberately misbehaving Burrow peer for
// adversarial tests. It reimplements the chain/AEAD/rekey math independently of
// internal/session so that interop with the specification is checked too. It
// is never linked into the binaries.
package testpeer

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"

	"github.com/guy5116/burrow/internal/handshake"
	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/wire"
)

// Chain is one direction of the symmetric ratchet.
type Chain struct {
	CK      []byte
	Epoch   uint32
	Counter uint64
}

// Next returns the message key and advances the chain (no counter cap: the
// test peer may deliberately exceed it).
func (c *Chain) Next() []byte {
	m := hmac.New(sha256.New, c.CK)
	m.Write([]byte{1})
	mk := m.Sum(nil)
	m.Reset()
	m.Write([]byte{2})
	c.CK = m.Sum(nil)
	c.Counter++
	return mk
}

// DeriveChains mirrors session.deriveChains.
func DeriveChains(root []byte, epoch uint32) (i2r, r2i []byte) {
	info := append([]byte(wire.ChainsInfo), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(info[len(wire.ChainsInfo):], epoch)
	okm, err := hkdf.Expand(sha256.New, root, string(info), 64)
	if err != nil {
		panic(err)
	}
	return okm[:32], okm[32:]
}

// Peer is the scriptable peer.
type Peer struct {
	Conn      net.Conn
	Initiator bool
	Root      []byte
	SendChain *Chain
	RecvChain *Chain
	// Static is the scalar used for rekey DHs; set to a wrong key to simulate an
	// attacker who holds session state but not the identity key.
	Static  []byte
	PeerPub identity.PeerID
	// Pending rekey ephemerals (initiator role).
	escalar  []byte
	kemSeed  []byte
	nextRecv *Chain
}

// New builds a peer sharing root with a session (no handshake).
func New(conn net.Conn, root []byte, initiator bool, static []byte, peer identity.PeerID) *Peer {
	p := &Peer{Conn: conn, Initiator: initiator, Root: append([]byte(nil), root...), Static: static, PeerPub: peer}
	i2r, r2i := DeriveChains(root, 0)
	if initiator {
		p.SendChain, p.RecvChain = &Chain{CK: i2r}, &Chain{CK: r2i}
	} else {
		p.SendChain, p.RecvChain = &Chain{CK: r2i}, &Chain{CK: i2r}
	}
	return p
}

// Dial performs an honest handshake as initiator and returns a raw peer.
func Dial(ctx context.Context, conn net.Conn, self *identity.Identity, peer identity.PeerID, token *[16]byte) (*Peer, error) {
	res, err := handshake.Initiate(ctx, conn, self, peer, token)
	if err != nil {
		return nil, err
	}
	p := New(conn, res.Root.Bytes(), true, self.Scalar(), peer)
	res.Root.Clear()
	return p, nil
}

// Accept performs an honest handshake as responder and returns a raw peer.
func Accept(ctx context.Context, conn net.Conn, self *identity.Identity, auth handshake.Authorizer) (*Peer, error) {
	res, err := handshake.Respond(ctx, conn, self, auth, nil)
	if err != nil {
		return nil, err
	}
	p := New(conn, res.Root.Bytes(), false, self.Scalar(), res.Peer)
	res.Root.Clear()
	return p, nil
}

func (p *Peer) sendDir() uint8 {
	if p.Initiator {
		return 0
	}
	return 1
}

// Seal encrypts an inner frame with the send chain and returns the outer frame.
func (p *Peer) Seal(inner []byte) []byte {
	counter := p.SendChain.Counter
	mk := p.SendChain.Next()
	aead, _ := chacha20poly1305.New(mk)
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], counter)
	ad := wire.FrameAD(nil, p.SendChain.Epoch, counter, p.sendDir())
	ct := aead.Seal(nil, nonce[:], inner, ad)
	out := make([]byte, 4, 4+len(ct))
	binary.BigEndian.PutUint32(out, uint32(len(ct))) // #nosec G115 -- test helper
	return append(out, ct...)
}

// SendFrame encodes and sends an honest frame.
func (p *Peer) SendFrame(typ wire.FrameType, stream uint16, payload []byte) error {
	inner, err := wire.EncodeInner(nil, typ, stream, payload)
	if err != nil {
		return err
	}
	return p.SendInner(inner)
}

// SendInner seals arbitrary inner bytes (no validation) and sends them.
func (p *Peer) SendInner(inner []byte) error {
	_, err := p.Conn.Write(p.Seal(inner))
	return err
}

// SendOuter sends a raw length prefix and body, unencrypted and unchecked.
func (p *Peer) SendOuter(length uint32, body []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], length)
	if _, err := p.Conn.Write(hdr[:]); err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	_, err := p.Conn.Write(body)
	return err
}

// Hello sends our HELLO.
func (p *Peer) Hello(h wire.Hello) error {
	b, err := wire.AppendHello(nil, h)
	if err != nil {
		return err
	}
	return p.SendFrame(wire.TypeHello, wire.StreamControl, b)
}

// Recv reads and decrypts one frame (honest side of the peer).
func (p *Peer) Recv(timeout time.Duration) (wire.Inner, error) {
	_ = p.Conn.SetReadDeadline(time.Now().Add(timeout))
	var hdr [4]byte
	if _, err := io.ReadFull(p.Conn, hdr[:]); err != nil {
		return wire.Inner{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > wire.MaxCiphertext {
		return wire.Inner{}, errors.New("testpeer: bad length")
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(p.Conn, ct); err != nil {
		return wire.Inner{}, err
	}
	counter := p.RecvChain.Counter
	mk := p.RecvChain.Next()
	aead, _ := chacha20poly1305.New(mk)
	var nonce [12]byte
	binary.BigEndian.PutUint64(nonce[4:], counter)
	ad := wire.FrameAD(nil, p.RecvChain.Epoch, counter, 1-p.sendDir())
	pt, err := aead.Open(nil, nonce[:], ct, ad)
	if err != nil {
		return wire.Inner{}, err
	}
	return wire.DecodeInner(pt)
}

// ExpectClose waits for the connection to be closed by the peer without any
// further frame. It returns an error if a frame arrives first or the timeout passes.
func (p *Peer) ExpectClose(timeout time.Duration) error {
	_ = p.Conn.SetReadDeadline(time.Now().Add(timeout))
	var b [1]byte
	n, err := p.Conn.Read(b[:])
	if n > 0 {
		return errors.New("testpeer: received data, expected silent close")
	}
	if err == nil {
		return errors.New("testpeer: read returned no data and no error")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("testpeer: connection still open after timeout")
	}
	return nil
}

// --- rekey ---

// StartRekey (initiator role) sends REKEY_INIT and remembers the ephemerals.
func (p *Peer) StartRekey() error {
	p.escalar = make([]byte, 32)
	_, _ = rand.Read(p.escalar)
	epub, _ := curve25519.X25519(p.escalar, curve25519.Basepoint)
	p.kemSeed = make([]byte, 64)
	_, _ = rand.Read(p.kemSeed)
	dk, _ := mlkem.NewDecapsulationKey768(p.kemSeed)
	var init wire.RekeyInit
	copy(init.EPub[:], epub)
	copy(init.KemEK[:], dk.EncapsulationKey().Bytes())
	return p.SendFrame(wire.TypeRekeyInit, wire.StreamControl, wire.AppendRekeyInit(nil, &init))
}

// FinishRekey (initiator role) consumes REKEY_RESP, switches RECV, sends DONE, switches SEND.
func (p *Peer) FinishRekey(resp []byte) error {
	var r wire.RekeyResp
	if err := wire.DecodeRekeyResp(resp, &r); err != nil {
		return err
	}
	dk, _ := mlkem.NewDecapsulationKey768(p.kemSeed)
	ss, err := dk.Decapsulate(r.KemCT[:])
	if err != nil {
		return err
	}
	i2r, r2i, err := p.deriveNext(p.escalar, r.EPub[:], p.escalar, p.PeerPub[:], p.Static, r.EPub[:], ss)
	if err != nil {
		return err
	}
	p.RecvChain = &Chain{CK: r2i, Epoch: p.RecvChain.Epoch + 1}
	if err := p.SendFrame(wire.TypeRekeyDone, wire.StreamControl, nil); err != nil {
		return err
	}
	p.SendChain = &Chain{CK: i2r, Epoch: p.SendChain.Epoch + 1}
	return nil
}

// AnswerRekey (responder role) consumes REKEY_INIT, sends RESP and switches SEND;
// RECV switches on the next REKEY_DONE via AcceptDone.
func (p *Peer) AnswerRekey(initPayload []byte) error {
	var init wire.RekeyInit
	if err := wire.DecodeRekeyInit(initPayload, &init); err != nil {
		return err
	}
	ek, err := mlkem.NewEncapsulationKey768(init.KemEK[:])
	if err != nil {
		return err
	}
	esc := make([]byte, 32)
	_, _ = rand.Read(esc)
	epub, _ := curve25519.X25519(esc, curve25519.Basepoint)
	ss, ct := ek.Encapsulate()
	i2r, r2i, err := p.deriveNext(esc, init.EPub[:], p.Static, init.EPub[:], esc, p.PeerPub[:], ss)
	if err != nil {
		return err
	}
	p.nextRecv = &Chain{CK: i2r, Epoch: p.RecvChain.Epoch + 1}
	var resp wire.RekeyResp
	copy(resp.EPub[:], epub)
	copy(resp.KemCT[:], ct)
	if err := p.SendFrame(wire.TypeRekeyResp, wire.StreamControl, wire.AppendRekeyResp(nil, &resp)); err != nil {
		return err
	}
	p.SendChain = &Chain{CK: r2i, Epoch: p.SendChain.Epoch + 1}
	return nil
}

// AcceptDone (responder role) switches RECV after REKEY_DONE.
func (p *Peer) AcceptDone() {
	p.RecvChain = p.nextRecv
	p.nextRecv = nil
}

func (p *Peer) deriveNext(eeS, eeP, esS, esP, seS, seP, ss []byte) (i2r, r2i []byte, err error) {
	ikm := make([]byte, 0, 128)
	for _, pair := range [][2][]byte{{eeS, eeP}, {esS, esP}, {seS, seP}} {
		dh, err := curve25519.X25519(pair[0], pair[1])
		if err != nil {
			return nil, nil, err
		}
		ikm = append(ikm, dh...)
	}
	ikm = append(ikm, ss...)
	prk, err := hkdf.Extract(sha256.New, ikm, p.Root)
	if err != nil {
		return nil, nil, err
	}
	p.Root = prk
	i2r, r2i = DeriveChains(p.Root, p.RecvChain.Epoch+1)
	return i2r, r2i, nil
}
