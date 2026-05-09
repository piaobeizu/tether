package watchdog

import (
	"sync/atomic"
	"time"
)

// heartbeat is the supervisor-side liveness probe. The supervisor hands
// hb.Beat to the subsystem's Run; the subsystem MUST call it
// periodically (the contract says "at least once every
// HeartbeatTimeout/2"). The supervisor reads LastBeat from a watcher
// goroutine and cancels the run if the gap exceeds HeartbeatTimeout.
//
// Implementation is lockless — Beat / LastBeat hit a single atomic
// int64 (unix nanos). createdAt is captured once at construction and
// is used by the supervisor to bound the time the subsystem may take
// to issue its first beat.
type heartbeat struct {
	createdAt time.Time
	lastNS    atomic.Int64
}

func newHeartbeat() *heartbeat {
	return &heartbeat{createdAt: time.Now()}
}

// Beat records the current time. Called by the supervised subsystem.
// Cheap: a single atomic store.
func (h *heartbeat) Beat() {
	h.lastNS.Store(time.Now().UnixNano())
}

// LastBeat returns the last beat time, or the zero time if Beat was
// never called.
func (h *heartbeat) LastBeat() time.Time {
	v := h.lastNS.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(0, v)
}
