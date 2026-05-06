package watchdog

import (
	"math"
	"sync"
	"time"
)

// Backoff is the supervisor restart backoff strategy. v0.1 uses
// exponential backoff with a configurable cap and a jitter-free
// schedule (single-process, single-supervisor — no thundering herd
// problem to solve).
//
// Sequence with default config (Initial=200ms, Max=10s, Factor=2):
//
//	200ms, 400ms, 800ms, 1.6s, 3.2s, 6.4s, 10s, 10s, 10s, ...
//
// Reset() returns the schedule to Initial; supervisors call it after a
// run survives longer than the resetThreshold (= 30s by default), so
// transient failures don't permanently inflate the delay.
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64

	mu      sync.Mutex
	current time.Duration
}

// NewBackoff constructs a backoff with the given parameters. Zero values
// fall back to defaults (Initial=200ms, Max=10s, Factor=2).
func NewBackoff(initial, max time.Duration, factor float64) *Backoff {
	if initial <= 0 {
		initial = 200 * time.Millisecond
	}
	if max <= 0 {
		max = 10 * time.Second
	}
	if factor < 1 {
		factor = 2
	}
	return &Backoff{Initial: initial, Max: max, Factor: factor}
}

// Next returns the next delay and advances the schedule. Concurrency-safe.
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current == 0 {
		b.current = b.Initial
		return b.current
	}
	next := time.Duration(math.Min(
		float64(b.Max),
		float64(b.current)*b.Factor,
	))
	b.current = next
	return b.current
}

// Reset rewinds the schedule. Supervisors call this after a successful
// run-window so transient panics don't ratchet the delay forever.
func (b *Backoff) Reset() {
	b.mu.Lock()
	b.current = 0
	b.mu.Unlock()
}
