package crypto

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultReplayWindow is the timestamp tolerance window. Per spec §11.C
// envelopes carry a `ts` metadata field; receivers reject envelopes whose
// ts is more than DefaultReplayWindow off from the local clock. 5 minutes
// matches the Kerberos-era convention of allowing real-world clock skew
// between mobile and daemon hosts.
const DefaultReplayWindow = 5 * time.Minute

// DefaultReplayCacheSize is the LRU dedup cache capacity. Sized to ~5
// minutes of envelope traffic at a generous 100 envelopes/sec.
const DefaultReplayCacheSize = 30000

// ErrReplay is returned when an envelope fails the replay check —
// already-seen ID, or timestamp outside the skew window. Callers MUST
// treat this as a hard rejection (do NOT decrypt; the envelope's authority
// has already been undermined).
var ErrReplay = errors.New("crypto: replay detected")

// ReplayGuard tracks recently-seen envelope IDs and the local notion of
// "now" for skew rejection. Concurrent-safe.
//
// Cross-Rust interop: the Mobile App runs an analogous guard with the same
// 5-minute window and same dedup window size. The wire field carrying the
// envelope ID (caller-supplied) is whatever the upper layer chooses —
// typically a UUIDv4 generated at SealEnvelope time and prepended to the
// outer transport frame. The replay guard does NOT itself read fields
// from the Envelope struct in xchacha20poly1305.go — the wire ID lives at
// the transport layer above this package.
type ReplayGuard struct {
	mu       sync.Mutex
	window   time.Duration
	capacity int
	now      func() time.Time
	// seen maps envelope ID → list element holding (id, time) for LRU eviction.
	seen  map[string]*list.Element
	order *list.List
}

type replayEntry struct {
	id        string
	receivedAt time.Time
}

// ReplayGuardOption configures NewReplayGuard. Accepting options as a
// variadic keeps the common-case (defaults) call site uncluttered while
// letting tests inject a deterministic clock.
type ReplayGuardOption func(*ReplayGuard)

// WithReplayWindow overrides the default 5-minute skew window.
func WithReplayWindow(d time.Duration) ReplayGuardOption {
	return func(g *ReplayGuard) { g.window = d }
}

// WithReplayCacheSize overrides the default LRU capacity.
func WithReplayCacheSize(n int) ReplayGuardOption {
	return func(g *ReplayGuard) { g.capacity = n }
}

// WithReplayClock injects a clock function — tests pass a fake to advance
// time deterministically.
func WithReplayClock(now func() time.Time) ReplayGuardOption {
	return func(g *ReplayGuard) { g.now = now }
}

// NewReplayGuard returns a ReplayGuard configured with the supplied options
// (or the package defaults).
func NewReplayGuard(opts ...ReplayGuardOption) *ReplayGuard {
	g := &ReplayGuard{
		window:   DefaultReplayWindow,
		capacity: DefaultReplayCacheSize,
		now:      time.Now,
		seen:     make(map[string]*list.Element),
		order:    list.New(),
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Check verifies that envelopeID has not been seen and that timestamp is
// within the skew window. On success it records envelopeID in the LRU
// cache (evicting the oldest entry if at capacity) and returns nil. On
// failure it returns ErrReplay wrapped with a reason — the envelope MUST
// be dropped, not decrypted.
//
// Concurrent calls with the same envelopeID race-correctly: exactly one
// call returns nil, the rest return ErrReplay.
func (g *ReplayGuard) Check(envelopeID string, timestamp time.Time) error {
	if envelopeID == "" {
		return fmt.Errorf("%w: empty envelope id", ErrReplay)
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// 1. Skew check — uses local "now" not the cache's recorded times so
	//    timestamps from far past or far future are immediately rejected.
	now := g.now()
	if timestamp.Before(now.Add(-g.window)) {
		return fmt.Errorf("%w: timestamp %s is more than %s in the past", ErrReplay, timestamp.Format(time.RFC3339Nano), g.window)
	}
	if timestamp.After(now.Add(g.window)) {
		return fmt.Errorf("%w: timestamp %s is more than %s in the future", ErrReplay, timestamp.Format(time.RFC3339Nano), g.window)
	}

	// 2. Garbage-collect entries older than the window — bounds memory
	//    independent of the LRU capacity if traffic is bursty.
	cutoff := now.Add(-g.window)
	for e := g.order.Front(); e != nil; {
		entry := e.Value.(*replayEntry)
		if entry.receivedAt.After(cutoff) {
			break
		}
		next := e.Next()
		g.order.Remove(e)
		delete(g.seen, entry.id)
		e = next
	}

	// 3. Dedup check.
	if _, ok := g.seen[envelopeID]; ok {
		return fmt.Errorf("%w: envelope %s already seen", ErrReplay, envelopeID)
	}

	// 4. Insert + LRU evict if needed.
	for g.order.Len() >= g.capacity {
		oldest := g.order.Front()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*replayEntry)
		g.order.Remove(oldest)
		delete(g.seen, entry.id)
	}
	elem := g.order.PushBack(&replayEntry{id: envelopeID, receivedAt: now})
	g.seen[envelopeID] = elem
	return nil
}

// Size returns the current number of cached envelope IDs (testing aid).
func (g *ReplayGuard) Size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.order.Len()
}
