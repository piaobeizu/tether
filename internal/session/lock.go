package session

import (
	"sync"
	"time"
)

const lockTimeout = 60 * time.Second

// SessionLock is a per-session input-gate mutex (D-15).
// Only one client at a time may hold the lock and send PTY input.
// Others may still receive events read-only via /wt/events.
type SessionLock struct {
	mu        sync.Mutex
	holder    string        // clientID of the lock holder ("" = unlocked)
	timer     *time.Timer
	preempted chan struct{}  // closed when the holder is force-taken
}

// Acquire attempts to acquire the lock for clientID.
// Returns (true, preempted) if acquired; (false, nil) if held by another client.
// The preempted channel is closed if ForceAcquire takes the lock from this holder.
func (l *SessionLock) Acquire(clientID string) (bool, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != "" && l.holder != clientID {
		return false, nil
	}
	l.holder = clientID
	l.preempted = make(chan struct{})
	l.resetTimerLocked()
	return true, l.preempted
}

// Release releases the lock if held by clientID (no-op otherwise).
func (l *SessionLock) Release(clientID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder == clientID {
		l.holder = ""
		if l.timer != nil {
			l.timer.Stop()
			l.timer = nil
		}
	}
}

// ForceAcquire takes the lock regardless of the current holder, preempting them.
// The existing holder's preempted channel is closed before the transfer.
// Returns a new preempted channel for the new holder.
func (l *SessionLock) ForceAcquire(clientID string) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.preempted != nil {
		close(l.preempted)
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	l.holder = clientID
	l.preempted = make(chan struct{})
	l.resetTimerLocked()
	return l.preempted
}

// Holder returns the current lock holder ID ("" = unlocked).
func (l *SessionLock) Holder() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holder
}

func (l *SessionLock) resetTimerLocked() {
	if l.timer != nil {
		l.timer.Stop()
	}
	preempted := l.preempted
	l.timer = time.AfterFunc(lockTimeout, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		// Only auto-release if the preempted channel hasn't changed (i.e. no
		// ForceAcquire raced us). Close the channel so the PTY handler unblocks
		// its <-preempted select and terminates — mirrors ForceAcquire behaviour.
		if l.preempted == preempted {
			close(l.preempted)
			l.preempted = nil
			l.holder = ""
			l.timer = nil
		}
	})
}
