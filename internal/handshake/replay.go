package handshake

import (
	"sync"
	"time"

	"github.com/guy5116/burrow/internal/wire"
)

// ReplayLRU remembers the last Msg1ReplayLRU initiator ephemerals (128 KiB).
type ReplayLRU struct {
	mu   sync.Mutex
	set  map[[wire.X25519Size]byte]struct{}
	ring [wire.Msg1ReplayLRU][wire.X25519Size]byte
	pos  int
	full bool
}

// NewReplayLRU allocates the LRU.
func NewReplayLRU() *ReplayLRU {
	return &ReplayLRU{set: make(map[[wire.X25519Size]byte]struct{}, wire.Msg1ReplayLRU)}
}

// Contains reports whether pub belongs to a msg1 that verified before. It is
// the cheap check made before any DH.
func (l *ReplayLRU) Contains(pub [wire.X25519Size]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.set[pub]
	return ok
}

// Add records the ephemeral of a msg1 that has verified, and reports false
// when it was already there (a replay that raced the first copy). Only
// verified messages are recorded: otherwise anyone, without the responder's
// key, could push the real entries out with garbage.
func (l *ReplayLRU) Add(pub [wire.X25519Size]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.set[pub]; ok {
		return false
	}
	if l.full {
		delete(l.set, l.ring[l.pos])
	}
	l.ring[l.pos] = pub
	l.set[pub] = struct{}{}
	l.pos++
	if l.pos == len(l.ring) {
		l.pos, l.full = 0, true
	}
	return true
}

// FailureLimiter counts handshake failures per key in a sliding window and
// penalizes a key that exceeds the limit (§3.4). Successful handshakes never
// consume budget. Keys are transport-specific (a /24, a /48, or "" for Tor).
type FailureLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	penalty time.Duration
	entries map[string]*limitEntry
}

type limitEntry struct {
	fails   []time.Time
	banned  time.Time
	touched time.Time
}

const maxLimiterKeys = 4096

// NewFailureLimiter allows max failures per window before a key is limited for penalty.
func NewFailureLimiter(max int, window, penalty time.Duration) *FailureLimiter {
	return &FailureLimiter{max: max, window: window, penalty: penalty, entries: map[string]*limitEntry{}}
}

// Limited reports whether key is currently penalized.
func (f *FailureLimiter) Limited(key string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[key]
	return e != nil && now.Before(e.banned)
}

// Fail records a failure for key; the caller checks Limited afterwards or on the next accept.
func (f *FailureLimiter) Fail(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[key]
	if e == nil {
		if len(f.entries) >= maxLimiterKeys {
			f.evict(now)
		}
		e = &limitEntry{}
		f.entries[key] = e
	}
	e.touched = now
	cut := now.Add(-f.window)
	kept := e.fails[:0]
	for _, t := range e.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.fails = append(kept, now)
	if len(e.fails) >= f.max {
		e.banned = now.Add(f.penalty)
		e.fails = e.fails[:0]
	}
}

// evict drops the stalest entries so the map stays bounded.
func (f *FailureLimiter) evict(now time.Time) {
	for k, e := range f.entries {
		if now.Sub(e.touched) > f.window && now.After(e.banned) {
			delete(f.entries, k)
		}
	}
	for k := range f.entries { // still full: drop arbitrary entries
		if len(f.entries) < maxLimiterKeys/2 {
			break
		}
		delete(f.entries, k)
	}
}
