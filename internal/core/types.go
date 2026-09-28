// Package core is the engine: sessions, peers, contacts, invites, the message
// queue and the event fan-out. It is the only package the UIs import.
package core

import (
	"time"

	"github.com/guy5116/burrow/internal/identity"
	"github.com/guy5116/burrow/internal/transport"
	"github.com/guy5116/burrow/internal/wire"
)

// PeerID is the peer's X25519 public key (declared in identity).
type PeerID = identity.PeerID

// MsgID identifies a TEXT message (random, from crypto/rand).
type MsgID uint64

// TransferID identifies an image transfer (Phase 2).
type TransferID [16]byte

// Config holds engine settings. Zero values select the defaults of CLAUDE.md §17.
type Config struct {
	ListenPort  uint16 // 0 → wire.DefaultListenPort
	DisplayName string // sent in HELLO; empty by default
	Typing      bool   // send/accept TYPING (off by default)
	NoTimestamp bool   // send sent_at = 0
	MaxPeers    int    // 0 → wire.MaxPeers
	// AutoReconnect keeps dialing contacts with a known address after a session drops.
	AutoReconnect bool
	// ListenAddr overrides the listen socket ("" → ":<ListenPort>"); tests use "127.0.0.1:0".
	ListenAddr string
}

// Contact is a saved peer.
type Contact struct {
	ID        PeerID
	Nickname  string
	Verified  bool
	Blocked   bool
	Addrs     []transport.Address
	FirstSeen time.Time
	InviteID  string
	Online    bool // live session (not persisted)
}

// Identity describes our own key for display.
type Identity struct {
	ID          PeerID
	Fingerprint string
	Display     string
}

// SafetyNumber is the 12-group verification string for one contact.
type SafetyNumber = identity.SafetyNumber

// InviteOptions parameterize CreateInvite.
type InviteOptions struct {
	Host     string         // address the peer should dial (IP literal, hostname, .onion)
	Port     uint16         // 0 → ListenPort
	Kind     transport.Kind // "" → tcp
	TTL      time.Duration  // 0 → wire.DefaultInviteTTL
	MultiUse bool
}

// Invite is an issued invite: the string to share and its metadata.
type Invite struct {
	ID       string
	String   string
	Expiry   time.Time
	MultiUse bool
}

// Target is what Connect dials: either an invite string or a saved contact.
// Name is a UI-level contact name resolved by the UI before Connect.
type Target struct {
	Invite  string
	Contact *PeerID
	Name    string
}

// Message status values.
type Status uint8

// Statuses.
const (
	StatusPending Status = iota
	StatusSent
	StatusDelivered
	StatusFailed
)

func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusSent:
		return "sent"
	case StatusDelivered:
		return "delivered"
	}
	return "failed"
}

// Event is one of the concrete event types below; UIs switch exhaustively.
type Event interface{ isEvent() }

// PeerConnected: a session is established (both HELLOs).
type PeerConnected struct {
	Peer      PeerID
	Initiator bool
	Transport transport.Kind
	Name      string // sanitized HELLO name (may be empty)
}

// PeerDisconnected: the session ended.
type PeerDisconnected struct {
	Peer   PeerID
	Reason string // redacted, user-facing
}

// Reconnecting: a reconnect attempt is scheduled.
type Reconnecting struct {
	Peer    PeerID
	Attempt int
	NextAt  time.Time
}

// HandshakeFailed: an outbound connection attempt failed.
type HandshakeFailed struct {
	Peer   PeerID // zero for an invite whose responder is unknown
	Stage  string
	Reason string
}

// NewPeerViaInvite: a contact was created from an invite (either side).
type NewPeerViaInvite struct {
	Peer     PeerID
	Nickname string
	InviteID string
}

// InviteConsumed: a token was used.
type InviteConsumed struct{ ID string }

// NameCollision: a new contact's name normalizes like an existing one.
type NameCollision struct {
	New      PeerID
	Existing PeerID
	Name     string
}

// PeerVerified: a contact was marked verified.
type PeerVerified struct{ Peer PeerID }

// MessageReceived: a TEXT from a peer (already sanitized).
type MessageReceived struct {
	Peer   PeerID
	ID     MsgID
	Text   string
	SentAt time.Time // zero when the peer disabled timestamps
}

// MessageStatus: a queued message changed state.
type MessageStatus struct {
	Peer   PeerID
	ID     MsgID
	Status Status
}

// Typing: peer typing indicator.
type Typing struct {
	Peer   PeerID
	Typing bool
}

// ImageOffered, TransferProgress, TransferResumed, TransferDone, TransferFailed arrive in Phase 2.
type ImageOffered struct {
	Peer PeerID
	ID   TransferID
}

// TransferProgress reports bytes transferred.
type TransferProgress struct {
	ID   TransferID
	Done uint64
	Size uint64
}

// TransferResumed reports a resumed transfer.
type TransferResumed struct{ ID TransferID }

// TransferDone reports completion.
type TransferDone struct {
	ID   TransferID
	Path string
}

// TransferFailed reports failure.
type TransferFailed struct {
	ID     TransferID
	Reason string
}

// ErrorEvent: a user-facing, already redacted error.
type ErrorEvent struct{ Message string }

func (PeerConnected) isEvent()    {}
func (PeerDisconnected) isEvent() {}
func (Reconnecting) isEvent()     {}
func (HandshakeFailed) isEvent()  {}
func (NewPeerViaInvite) isEvent() {}
func (InviteConsumed) isEvent()   {}
func (NameCollision) isEvent()    {}
func (PeerVerified) isEvent()     {}
func (MessageReceived) isEvent()  {}
func (MessageStatus) isEvent()    {}
func (Typing) isEvent()           {}
func (ImageOffered) isEvent()     {}
func (TransferProgress) isEvent() {}
func (TransferResumed) isEvent()  {}
func (TransferDone) isEvent()     {}
func (TransferFailed) isEvent()   {}
func (ErrorEvent) isEvent()       {}

func (c Config) listenPort() uint16 {
	if c.ListenPort == 0 {
		return wire.DefaultListenPort
	}
	return c.ListenPort
}

func (c Config) maxPeers() int {
	if c.MaxPeers == 0 {
		return wire.MaxPeers
	}
	return c.MaxPeers
}
