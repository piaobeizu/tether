package claude

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// OOM circuit-breaker (ops-concerns subitem #2 / gh-13 round-1 Tier-4 finding,
// piaobeizu/tether#22).
//
// Problem: when the cc subprocess is killed by the kernel OOM-killer (Linux
// SIGKILL → exit code -1 with WaitStatus.Signal()=SIGKILL), Recover() will
// dutifully re-spawn it — and immediately get OOM-killed again, looping
// forever. The daemon spins, the user sees "session keeps crashing" with
// no clean failure mode.
//
// Mechanism:
//   1. Recover detects whether the subprocess died ON ITS OWN (vs being
//      killed by us during the Recover teardown). The pre-kill liveness
//      probe in session.go's Recover() determines this.
//   2. If the subprocess died on its own AND its exit was a SIGKILL, we
//      attribute it to the OOM-killer (or an external `kill -9`) and
//      append a timestamp to oomEvents.
//   3. Recover's first action consults oomCircuitTripped(): if N OOM
//      events have occurred within the policy window, return
//      ErrOOMCircuitOpen instead of attempting another doomed spawn.
//
// Defaults: 3 OOM exits within 60 seconds → trip. Tunable via SpawnOpts.

// Default circuit policy.
const (
	DefaultOOMCircuitMaxExits = 3
	DefaultOOMCircuitWindow   = 60 * time.Second
)

// ErrOOMCircuitOpen is returned by Recover when the OOM circuit-breaker has
// tripped. The daemon supervisor should surface a clean degrade-gracefully
// message to the user instead of looping further Recover attempts.
var ErrOOMCircuitOpen = errors.New("claude: OOM circuit-breaker open (subprocess killed too often)")

// isOOMExit returns true when err looks like a SIGKILL-induced exit. We use
// this AFTER we've established that we did NOT kill the subprocess
// ourselves (the liveness probe in Recover sets that flag) — so a true here
// means kernel OOM-killer or external `kill -9`.
//
// On Linux, SIGKILL produces ExitCode == -1 and WaitStatus.Signal() == SIGKILL.
// On non-Unix or if the wait status type assertion fails, return false (no
// false positives).
func isOOMExit(err error) bool {
	if err == nil {
		return false
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ProcessState == nil {
		return false
	}
	ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// recordOOMEvent appends time.Now() to the OOM event log. Caller MUST hold
// s.mu in exclusive (Lock) mode.
func (s *Session) recordOOMEventLocked(now time.Time) {
	s.oomEvents = append(s.oomEvents, now)
	// Garbage-collect events outside the policy window so the slice
	// doesn't grow without bound.
	cutoff := now.Add(-s.oomWindow())
	keep := s.oomEvents[:0]
	for _, t := range s.oomEvents {
		if !t.Before(cutoff) {
			keep = append(keep, t)
		}
	}
	s.oomEvents = keep
}

// oomCircuitTrippedLocked reports whether enough OOM events have accumulated
// within the policy window to trip the circuit-breaker. Caller MUST hold
// s.mu in shared (RLock) or exclusive mode.
func (s *Session) oomCircuitTrippedLocked(now time.Time) bool {
	max := s.oomMaxExits()
	if max <= 0 {
		return false // disabled
	}
	cutoff := now.Add(-s.oomWindow())
	n := 0
	for _, t := range s.oomEvents {
		if !t.Before(cutoff) {
			n++
		}
	}
	return n >= max
}

// OOMExits returns a copy of OOM event timestamps recorded for this Session
// (within the current window — older events are pruned on each record).
// Used by daemon-side observability + tests.
func (s *Session) OOMExits() []time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.oomEvents) == 0 {
		return nil
	}
	out := make([]time.Time, len(s.oomEvents))
	copy(out, s.oomEvents)
	return out
}

// oomMaxExits / oomWindow return the configured policy with default fallback.
// Caller does NOT need to hold s.mu (these read SpawnOpts which is immutable
// after New).
//
// Tri-state semantics:
//
//	> 0  use the configured threshold
//	== 0 use DefaultOOMCircuitMaxExits
//	< 0  disabled — return as-is so oomCircuitTrippedLocked's
//	     `if max <= 0 { return false }` short-circuits and the circuit
//	     never trips regardless of how many events accumulate
func (s *Session) oomMaxExits() int {
	if s.spawnOpts.OOMCircuitMaxExits == 0 {
		return DefaultOOMCircuitMaxExits
	}
	return s.spawnOpts.OOMCircuitMaxExits
}

func (s *Session) oomWindow() time.Duration {
	if s.spawnOpts.OOMCircuitWindow > 0 {
		return s.spawnOpts.OOMCircuitWindow
	}
	return DefaultOOMCircuitWindow
}
