package pair

import "time"

// Timeouts per spec §7. Single-shot, start-on-state-entry. The driver
// (client.go / server.go) creates a timer when transitioning into a
// timed state and cancels it on next state change.
const (
	TimeoutAwaitingPubkey = 30 * time.Second
	TimeoutSASConfirm     = 60 * time.Second
	TimeoutCompleting     = 10 * time.Second
)

// TimeoutForState returns the timer that should be armed when entering
// state. Returns 0 for states that have no timeout.
func TimeoutForState(s State) time.Duration {
	switch s {
	case StateAwaitingPubkey:
		return TimeoutAwaitingPubkey
	case StateSASConfirm:
		return TimeoutSASConfirm
	case StateCompleting:
		return TimeoutCompleting
	default:
		return 0
	}
}
