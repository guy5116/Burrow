package handshake

import (
	"context"
	"crypto/hkdf"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/flynn/noise"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/secret"
	"github.com/guy5116/burrow/internal/wire"
)

// Decision is the authorization verdict for an initiator.
type Decision uint8

// Decisions.
const (
	Reject Decision = iota
	Accept
)

// Authorizer decides whether an authenticated initiator may proceed. token is
// nil when msg3 carried none. It must not block on the network.
type Authorizer func(peer identity.PeerID, token *[wire.InviteTokenSz]byte) Decision

// Result is a completed handshake. Root is root_0 (32 bytes); the caller owns
// it and clears it when the session ends.
type Result struct {
	Peer      identity.PeerID
	Root      *secret.Buffer
	Initiator bool
	// Token is the invite token the initiator presented (responder side only).
	Token *[wire.InviteTokenSz]byte
}

// Error reports the stage at which a handshake failed. Err may be a network
// error that names the remote address: log the Stage, never the text.
type Error struct {
	Stage string
	Err   error
}

func (e *Error) Error() string { return "handshake: " + e.Stage + ": " + e.Err.Error() }
func (e *Error) Unwrap() error { return e.Err }

// Sentinel causes.
var (
	ErrLength     = errors.New("handshake message has illegal length")
	ErrReplay     = errors.New("msg1 ephemeral seen before")
	ErrRejected   = errors.New("initiator not authorized")
	ErrBadKEM     = errors.New("invalid ML-KEM encapsulation key")
	ErrBadPayload = errors.New("malformed handshake payload")
	ErrTimeout    = errors.New("handshake timed out")
)

var suite = noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)

func fail(stage string, err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		err = ErrTimeout
	}
	return &Error{Stage: stage, Err: err}
}

// Initiate runs the initiator side over conn. The responder's key must be
// known (peer). token is sent in msg3 when non-nil. Nothing is written on
// failure after the first message; the caller closes conn.
func Initiate(ctx context.Context, conn net.Conn, self *identity.Identity, peer identity.PeerID, token *[wire.InviteTokenSz]byte) (*Result, error) {
	stop := armDeadline(ctx, conn, wire.HandshakeTimeout)
	defer stop()

	pub := self.Public()
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Pattern: noise.HandshakeXK, Initiator: true, Random: rand.Reader,
		Prologue:      []byte(wire.Prologue),
		StaticKeypair: noise.DHKey{Private: self.Scalar(), Public: pub[:]},
		PeerStatic:    peer[:],
	})
	if err != nil {
		return nil, fail("init", err)
	}
	defer wipeEphemeral(hs)

	kemSeed, err := secret.Random(wire.KEMSeedSize)
	if err != nil {
		return nil, fail("init", err)
	}
	defer kemSeed.Clear()
	kemDK, err := mlkem.NewDecapsulationKey768(kemSeed.Bytes())
	if err != nil {
		return nil, fail("init", err)
	}
	var hs1 wire.HS1
	copy(hs1.KemEK[:], kemDK.EncapsulationKey().Bytes())

	buf := make([]byte, wire.HSLenPrefix, wire.HSLenPrefix+wire.HS1Len)
	msg1, _, _, err := hs.WriteMessage(buf, wire.AppendHS1(nil, &hs1))
	if err != nil {
		return nil, fail("msg1", err)
	}
	if err := writeHS(conn, msg1, wire.HS1Len); err != nil {
		return nil, fail("msg1", err)
	}

	msg2, err := readHS(conn, wire.HS2Len)
	if err != nil {
		return nil, fail("msg2", err)
	}
	pt2, _, _, err := hs.ReadMessage(nil, msg2)
	if err != nil {
		return nil, fail("msg2", err)
	}
	var hs2 wire.HS2
	if err := wire.DecodeHS2(pt2, &hs2); err != nil {
		return nil, fail("msg2", ErrBadPayload)
	}
	secret.Wipe(pt2)
	kemSS, err := kemDK.Decapsulate(hs2.KemCT[:])
	if err != nil {
		return nil, fail("msg2", err)
	}
	ss := secret.From(kemSS)
	defer ss.Clear()

	ri, err := secret.Random(wire.HSRandomSize)
	if err != nil {
		return nil, fail("msg3", err)
	}
	defer ri.Clear()
	hs3 := wire.HS3{HasToken: token != nil}
	copy(hs3.R[:], ri.Bytes())
	if token != nil {
		hs3.Token = *token
	}
	pt3 := wire.AppendHS3(nil, &hs3)
	defer secret.Wipe(pt3)
	hs3.R = [wire.HSRandomSize]byte{}
	buf = make([]byte, wire.HSLenPrefix, wire.HSLenPrefix+wire.HS3Len)
	msg3, _, _, err := hs.WriteMessage(buf, pt3)
	if err != nil {
		return nil, fail("msg3", err)
	}
	if err := writeHS(conn, msg3, wire.HS3Len); err != nil {
		return nil, fail("msg3", err)
	}

	rr := secret.From(hs2.R[:])
	defer rr.Clear()
	root, err := deriveRoot(hs.ChannelBinding(), ri, rr, ss)
	if err != nil {
		return nil, fail("derive", err)
	}
	return &Result{Peer: peer, Root: root, Initiator: true}, nil
}

// Respond runs the responder side. On any failure it returns without having
// written anything after msg2 (and nothing at all before msg1 verifies); the
// caller closes conn silently. replay may be nil (tests only).
func Respond(ctx context.Context, conn net.Conn, self *identity.Identity, auth Authorizer, replay *ReplayLRU) (*Result, error) {
	start := time.Now()
	stop := armDeadline(ctx, conn, wire.HandshakeTimeout)
	defer stop()
	if err := conn.SetReadDeadline(start.Add(wire.HandshakeMsg1Wait)); err != nil {
		return nil, fail("msg1", err)
	}
	msg1, err := readHS(conn, wire.HS1Len)
	if err != nil {
		return nil, fail("msg1", err)
	}
	// Back to the one deadline for the whole handshake, counted from accept.
	if err := conn.SetReadDeadline(start.Add(wire.HandshakeTimeout)); err != nil {
		return nil, fail("msg1", err)
	}
	if err := ctx.Err(); err != nil { // the line above may have undone a cancellation
		return nil, fail("msg1", err)
	}
	// Cost ordering: replay check (map lookup) before any DH.
	var epub [wire.X25519Size]byte
	copy(epub[:], msg1)
	if replay != nil && replay.Contains(epub) {
		return nil, fail("msg1", ErrReplay)
	}

	pub := self.Public()
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite: suite, Pattern: noise.HandshakeXK, Random: rand.Reader,
		Prologue:      []byte(wire.Prologue),
		StaticKeypair: noise.DHKey{Private: self.Scalar(), Public: pub[:]},
	})
	if err != nil {
		return nil, fail("init", err)
	}
	defer wipeEphemeral(hs)

	pt1, _, _, err := hs.ReadMessage(nil, msg1)
	if err != nil {
		return nil, fail("msg1", err)
	}
	if replay != nil && !replay.Add(epub) {
		return nil, fail("msg1", ErrReplay)
	}
	var hs1 wire.HS1
	if err := wire.DecodeHS1(pt1, &hs1); err != nil {
		return nil, fail("msg1", ErrBadPayload)
	}
	kemEK, err := mlkem.NewEncapsulationKey768(hs1.KemEK[:])
	if err != nil {
		return nil, fail("msg1", ErrBadKEM)
	}
	kemSS, kemCT := kemEK.Encapsulate()
	ss := secret.From(kemSS)
	defer ss.Clear()
	rr, err := secret.Random(wire.HSRandomSize)
	if err != nil {
		return nil, fail("msg2", err)
	}
	defer rr.Clear()
	var hs2 wire.HS2
	copy(hs2.KemCT[:], kemCT)
	copy(hs2.R[:], rr.Bytes())
	pt2 := wire.AppendHS2(nil, &hs2)
	hs2.R = [wire.HSRandomSize]byte{}
	buf := make([]byte, wire.HSLenPrefix, wire.HSLenPrefix+wire.HS2Len)
	msg2, _, _, err := hs.WriteMessage(buf, pt2)
	secret.Wipe(pt2)
	if err != nil {
		return nil, fail("msg2", err)
	}
	if err := writeHS(conn, msg2, wire.HS2Len); err != nil {
		return nil, fail("msg2", err)
	}

	msg3, err := readHS(conn, wire.HS3Len)
	if err != nil {
		return nil, fail("msg3", err)
	}
	pt3, _, _, err := hs.ReadMessage(nil, msg3)
	if err != nil {
		return nil, fail("msg3", err)
	}
	var hs3 wire.HS3
	if err := wire.DecodeHS3(pt3, &hs3); err != nil {
		return nil, fail("msg3", ErrBadPayload)
	}
	secret.Wipe(pt3)
	ri := secret.From(hs3.R[:])
	defer ri.Clear()

	var peer identity.PeerID
	copy(peer[:], hs.PeerStatic())
	var token *[wire.InviteTokenSz]byte
	if hs3.HasToken {
		t := hs3.Token
		token = &t
	}
	if auth == nil || auth(peer, token) != Accept {
		return nil, fail("authorize", ErrRejected)
	}
	root, err := deriveRoot(hs.ChannelBinding(), ri, rr, ss)
	if err != nil {
		return nil, fail("derive", err)
	}
	return &Result{Peer: peer, Root: root, Token: token}, nil
}

// deriveRoot: root_0 = HKDF-Extract(sha256, r_i || r_r || kem_ss, salt = h).
func deriveRoot(h []byte, ri, rr, ss *secret.Buffer) (*secret.Buffer, error) {
	ikm := secret.New(3 * 32)
	defer ikm.Clear()
	b := ikm.Bytes()
	copy(b[:32], ri.Bytes())
	copy(b[32:64], rr.Bytes())
	copy(b[64:], ss.Bytes())
	prk, err := hkdf.Extract(sha256.New, b, h)
	if err != nil {
		return nil, err
	}
	return secret.From(prk), nil
}

// wipeEphemeral zeros the ephemeral scalar flynn/noise generated inside the
// handshake state (its LocalEphemeral aliases the library's slice).
func wipeEphemeral(hs *noise.HandshakeState) { secret.Wipe(hs.LocalEphemeral().Private) }

// armDeadline applies a hard deadline and also fires it when ctx ends.
func armDeadline(ctx context.Context, conn net.Conn, d time.Duration) func() {
	_ = conn.SetDeadline(time.Now().Add(d))
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	return func() {
		stop()
		_ = conn.SetDeadline(time.Time{})
	}
}

// writeHS writes u16 BE length || message; buf already has 2 spare leading bytes.
func writeHS(conn net.Conn, buf []byte, want int) error {
	if len(buf)-wire.HSLenPrefix != want {
		return fmt.Errorf("%w: produced %d", ErrLength, len(buf)-wire.HSLenPrefix)
	}
	binary.BigEndian.PutUint16(buf, uint16(want)) // #nosec G115 -- ≤ 1232
	_, err := conn.Write(buf)
	return err
}

// readHS reads u16 BE length || message and rejects any length but want
// before reading the body.
func readHS(conn net.Conn, want int) ([]byte, error) {
	var hdr [wire.HSLenPrefix]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	if int(binary.BigEndian.Uint16(hdr[:])) != want {
		return nil, ErrLength
	}
	msg := make([]byte, want)
	if _, err := io.ReadFull(conn, msg); err != nil {
		return nil, err
	}
	return msg, nil
}
