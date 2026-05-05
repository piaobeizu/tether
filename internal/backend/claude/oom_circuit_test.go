package claude

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// ───────── isOOMExit detector tests ─────────

// nil error is not an OOM exit.
func TestIsOOMExit_NilNotOOM(t *testing.T) {
	if isOOMExit(nil) {
		t.Errorf("isOOMExit(nil) should be false")
	}
}

// Plain error (not exec.ExitError) is not an OOM exit.
func TestIsOOMExit_NonExitErrorNotOOM(t *testing.T) {
	if isOOMExit(errors.New("random error")) {
		t.Errorf("isOOMExit on non-ExitError should be false")
	}
}

// Real subprocess that self-SIGKILLs → isOOMExit must be true.
//
// The whole point of OOM detection is recognizing a SIGKILL exit, regardless
// of who sent the signal — kernel OOM-killer or `kill -9`. We use shell
// self-kill as the most portable simulation.
func TestIsOOMExit_RealSubprocessSelfSIGKILL(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("isOOMExit relies on syscall.WaitStatus; %s untested here", runtime.GOOS)
	}
	cmd := exec.Command("sh", "-c", "kill -9 $$")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	err := cmd.Wait()
	if !isOOMExit(err) {
		t.Errorf("expected isOOMExit=true for self-SIGKILL'd subprocess; got err=%v", err)
	}
}

// Subprocess that exits cleanly (status 0) → not OOM.
func TestIsOOMExit_CleanExitNotOOM(t *testing.T) {
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("/bin/true should exit 0; got %v", err)
	}
	if isOOMExit(nil) {
		t.Errorf("clean exit should not be OOM")
	}
}

// Subprocess that exits non-zero (status 1) → not OOM (it's an exit code,
// not a signal).
func TestIsOOMExit_NonZeroExitNotOOM(t *testing.T) {
	cmd := exec.Command("/bin/false")
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}
	err := cmd.Run()
	if err == nil {
		t.Fatal("/bin/false should error")
	}
	if isOOMExit(err) {
		t.Errorf("status-1 exit should NOT be classified as OOM; got: %v", err)
	}
}

// ───────── circuit policy tests ─────────

func oomTestSession(t *testing.T, maxExits int, window time.Duration) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sess, err := New(ctx, SpawnOpts{
		BinaryPath:         "/bin/cat",
		OOMCircuitMaxExits: maxExits,
		OOMCircuitWindow:   window,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	go drainEvents(sess)
	return sess
}

// Defaults: 0 events → not tripped.
func TestOOMCircuit_FreshSessionNotTripped(t *testing.T) {
	sess := oomTestSession(t, 0, 0) // 0 = use defaults (3 / 60s)
	sess.mu.RLock()
	tripped := sess.oomCircuitTrippedLocked(time.Now())
	sess.mu.RUnlock()
	if tripped {
		t.Errorf("fresh session should not be tripped")
	}
	if exits := sess.OOMExits(); len(exits) != 0 {
		t.Errorf("expected 0 OOM events, got %d", len(exits))
	}
}

// Exactly N-1 events within window → still not tripped; N → tripped.
func TestOOMCircuit_TripsExactlyAtThreshold(t *testing.T) {
	sess := oomTestSession(t, 3, time.Minute)
	now := time.Now()
	sess.mu.Lock()
	sess.recordOOMEventLocked(now)
	sess.recordOOMEventLocked(now)
	tripped2 := sess.oomCircuitTrippedLocked(now)
	sess.recordOOMEventLocked(now)
	tripped3 := sess.oomCircuitTrippedLocked(now)
	sess.mu.Unlock()
	if tripped2 {
		t.Errorf("2 events should not trip a max=3 circuit")
	}
	if !tripped3 {
		t.Errorf("3 events at max=3 should trip the circuit")
	}
	if got := len(sess.OOMExits()); got != 3 {
		t.Errorf("expected 3 events recorded, got %d", got)
	}
}

// Events outside the window are pruned and don't trip.
func TestOOMCircuit_OldEventsPruned(t *testing.T) {
	sess := oomTestSession(t, 3, 10*time.Second)
	now := time.Now()
	sess.mu.Lock()
	// Record 3 events that are 1 minute old — all outside the 10s window.
	old := now.Add(-1 * time.Minute)
	sess.recordOOMEventLocked(old)
	sess.recordOOMEventLocked(old)
	sess.recordOOMEventLocked(old)
	sess.mu.Unlock()

	// recordOOMEventLocked prunes during its own write — but we recorded
	// stale timestamps. Fast-forward by recording one fresh event: pruning
	// happens against `now`, which is the recorded time. Since old events
	// were stamped 1min ago vs window=10s, they should be pruned out on
	// the FRESH event's recording (now > cutoff = now-10s).
	sess.mu.Lock()
	sess.recordOOMEventLocked(now)
	tripped := sess.oomCircuitTrippedLocked(now)
	sess.mu.Unlock()

	if tripped {
		t.Errorf("3 old events outside window + 1 fresh should NOT trip max=3 circuit")
	}
	exits := sess.OOMExits()
	if len(exits) != 1 {
		t.Errorf("expected 1 surviving event after pruning, got %d", len(exits))
	}
}

// MaxExits<0 disables the circuit entirely (Recover keeps re-spawning).
func TestOOMCircuit_NegativeMaxExitsDisables(t *testing.T) {
	sess := oomTestSession(t, -1, time.Second)
	now := time.Now()
	sess.mu.Lock()
	for i := 0; i < 100; i++ {
		sess.recordOOMEventLocked(now)
	}
	tripped := sess.oomCircuitTrippedLocked(now)
	sess.mu.Unlock()
	if tripped {
		t.Errorf("negative maxExits should disable the circuit; got tripped after 100 events")
	}
}

// OOMExits returns a copy — caller mutation must not leak.
func TestOOMCircuit_OOMExitsReturnsCopy(t *testing.T) {
	sess := oomTestSession(t, 3, time.Minute)
	t0 := time.Now()
	sess.mu.Lock()
	sess.recordOOMEventLocked(t0)
	sess.mu.Unlock()
	exits := sess.OOMExits()
	if len(exits) != 1 {
		t.Fatalf("expected 1 event, got %d", len(exits))
	}
	exits[0] = time.Time{} // tamper

	exits2 := sess.OOMExits()
	if exits2[0].Equal(time.Time{}) {
		t.Errorf("OOMExits must return a copy; caller mutation leaked")
	}
}

// ───────── Recover integration test ─────────

// When the circuit is already tripped, Recover() returns ErrOOMCircuitOpen
// immediately — does NOT proceed to teardown / re-spawn.
func TestSession_Recover_OOMCircuitOpen(t *testing.T) {
	sess := oomTestSession(t, 1, time.Minute) // very strict: 1 event = trip

	// Pretend Start happened (Recover gates on sid != "").
	sess.mu.Lock()
	sess.sessionID = "fake-sid"
	sess.recordOOMEventLocked(time.Now())
	sess.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := sess.Recover(ctx)
	if !errors.Is(err, ErrOOMCircuitOpen) {
		t.Errorf("expected ErrOOMCircuitOpen, got: %v", err)
	}
}
