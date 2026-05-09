package claude

// Tests for the F-07 cancel-in-flight sequence (spec §5.3 + Appendix B).
// Verifies both the byte order (Esc THEN Ctrl+U, never reversed) and
// the inter-keystroke pacing (cc needs ~500ms to process Esc before
// Ctrl+U is safe). PoC-1 documented this empirically; we just lock it
// in as code so a future drive-by edit can't silently regress.

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// timestampedWriter records every Write call with a monotonic
// timestamp, so a test can assert "byte X arrived before byte Y" AND
// "the gap between the two writes was at least D".
type timestampedWriter struct {
	mu     sync.Mutex
	start  time.Time
	writes []timestampedWrite
	// failOn, if non-nil, returns its value as the Write error when the
	// invocation count matches its index. Used to test the "skip the
	// second keystroke when the first fails" branch.
	failOn map[int]error
	calls  int
}

type timestampedWrite struct {
	atOffset time.Duration // since the writer was created
	bytes    []byte
}

func newTimestampedWriter() *timestampedWriter {
	return &timestampedWriter{start: time.Now()}
}

func (w *timestampedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	if err, ok := w.failOn[w.calls-1]; ok && err != nil {
		return 0, err
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	w.writes = append(w.writes, timestampedWrite{
		atOffset: time.Since(w.start),
		bytes:    cp,
	})
	return len(p), nil
}

// recordedSleeper captures the durations passed to time.Sleep so the
// test can assert "we asked for at least Esc→CtrlU then settle". Using
// a recorded fake instead of real sleeps keeps the test fast (-race
// -count=10 should still finish in well under a second).
type recordedSleeper struct {
	mu        sync.Mutex
	durations []time.Duration
	// realDelay, if >0, makes the sleeper actually sleep this long
	// each call so the timestampedWriter's offset measurements
	// reflect ordering, not just declared intent. We keep it small
	// (a few ms) to stay test-fast.
	realDelay time.Duration
}

func (s *recordedSleeper) Sleep(d time.Duration) {
	s.mu.Lock()
	s.durations = append(s.durations, d)
	delay := s.realDelay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
}

// newPTYSessionForCancelTest builds a PTYSession with NO subprocess
// and NO read loop — only the fields CancelInFlight touches. This is
// the lightest possible harness; spawning real cc here would make the
// test depend on a binary on PATH and obscure the byte-level assertions.
func newPTYSessionForCancelTest(w *timestampedWriter, sleep func(time.Duration)) *PTYSession {
	return &PTYSession{
		subscribers:  make(map[*ptySubscriber]struct{}),
		doneCh:       make(chan struct{}),
		cancelWriter: w,
		cancelSleep:  sleep,
	}
}

// TestCancelInFlight_ByteOrderAndSleeps is the spec-§5.3 lock-in:
// Esc (0x1b) MUST be the first byte written, Ctrl+U (0x15) MUST be the
// second, and there MUST be a real sleep gap between them.
func TestCancelInFlight_ByteOrderAndSleeps(t *testing.T) {
	w := newTimestampedWriter()
	sleeper := &recordedSleeper{realDelay: 5 * time.Millisecond}
	sess := newPTYSessionForCancelTest(w, sleeper.Sleep)

	if err := sess.CancelInFlight(); err != nil {
		t.Fatalf("CancelInFlight: %v", err)
	}

	// --- Byte order ---
	if len(w.writes) != 2 {
		t.Fatalf("got %d writes, want 2; writes=%+v", len(w.writes), w.writes)
	}
	if len(w.writes[0].bytes) != 1 || w.writes[0].bytes[0] != 0x1b {
		t.Errorf("first write = %v, want [0x1b] (Esc)", w.writes[0].bytes)
	}
	if len(w.writes[1].bytes) != 1 || w.writes[1].bytes[0] != 0x15 {
		t.Errorf("second write = %v, want [0x15] (Ctrl+U)", w.writes[1].bytes)
	}

	// --- Sleep durations: we expect the spec-mandated pair, in order. ---
	if got, want := len(sleeper.durations), 2; got != want {
		t.Fatalf("got %d sleeps, want %d; durations=%v", got, want, sleeper.durations)
	}
	if sleeper.durations[0] != CancelEscToCtrlUDelay {
		t.Errorf("first sleep = %v, want CancelEscToCtrlUDelay=%v",
			sleeper.durations[0], CancelEscToCtrlUDelay)
	}
	if sleeper.durations[1] != CancelCtrlUSettleDelay {
		t.Errorf("second sleep = %v, want CancelCtrlUSettleDelay=%v",
			sleeper.durations[1], CancelCtrlUSettleDelay)
	}

	// --- Sleep actually elapsed between writes. ---
	// realDelay above is 5ms per Sleep call, so the gap from write[0]
	// to write[1] should be ≥ 5ms (the first sleep). Use a generous
	// floor (3ms) to absorb scheduler jitter on -race builds.
	gap := w.writes[1].atOffset - w.writes[0].atOffset
	if gap < 3*time.Millisecond {
		t.Errorf("gap between Esc and Ctrl+U = %v, want ≥3ms (sleeper should have blocked)", gap)
	}

	// --- Spec sanity: the named delays land inside the spec windows
	// from §5.3 / F-07 (500–800ms, 200–500ms). Pin the upper bound too
	// so a future "let's just bump these to 5s" PR fails review here. ---
	if CancelEscToCtrlUDelay < 500*time.Millisecond || CancelEscToCtrlUDelay > 800*time.Millisecond {
		t.Errorf("CancelEscToCtrlUDelay=%v outside spec §5.3 window 500–800ms",
			CancelEscToCtrlUDelay)
	}
	if CancelCtrlUSettleDelay < 200*time.Millisecond || CancelCtrlUSettleDelay > 500*time.Millisecond {
		t.Errorf("CancelCtrlUSettleDelay=%v outside spec §5.3 window 200–500ms",
			CancelCtrlUSettleDelay)
	}
}

// TestCancelInFlight_FirstWriteFailsSkipsSecond locks in the safety
// behavior: if the Esc write fails, we MUST NOT push Ctrl+U onto the
// PTY (a stray Ctrl+U with no preceding Esc would clear whatever the
// user is currently typing).
func TestCancelInFlight_FirstWriteFailsSkipsSecond(t *testing.T) {
	wantErr := errors.New("simulated PTY write fail")
	w := newTimestampedWriter()
	w.failOn = map[int]error{0: wantErr}
	sleeper := &recordedSleeper{}
	sess := newPTYSessionForCancelTest(w, sleeper.Sleep)

	err := sess.CancelInFlight()
	if !errors.Is(err, wantErr) {
		t.Fatalf("CancelInFlight err = %v, want %v", err, wantErr)
	}
	if len(w.writes) != 0 {
		t.Errorf("got %d successful writes, want 0; writes=%+v", len(w.writes), w.writes)
	}
	if len(sleeper.durations) != 0 {
		t.Errorf("got %d sleeps after failed first write, want 0", len(sleeper.durations))
	}
}

// TestCancelInFlight_ClosedSession returns ErrSessionClosed without
// touching the writer or the sleeper — so a "stop" pressed against a
// torn-down session is a cheap no-op, not a panic.
func TestCancelInFlight_ClosedSession(t *testing.T) {
	w := newTimestampedWriter()
	sleeper := &recordedSleeper{}
	sess := newPTYSessionForCancelTest(w, sleeper.Sleep)
	sess.closed = true

	err := sess.CancelInFlight()
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("CancelInFlight on closed session: got %v, want ErrSessionClosed", err)
	}
	if len(w.writes) != 0 {
		t.Errorf("closed session still wrote %d times", len(w.writes))
	}
	if len(sleeper.durations) != 0 {
		t.Errorf("closed session still slept %d times", len(sleeper.durations))
	}
}
