package claude

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// stderrTestSession spins a Session backed by /bin/cat just to get a valid
// Session struct, then we drive appendStderr() directly. drainStderr is
// running on cat's stderr (which is empty), so it doesn't interfere.
func stderrTestSession(t *testing.T) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sess, err := New(ctx, SpawnOpts{BinaryPath: "/bin/cat"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	go drainEvents(sess)
	return sess
}

// Small writes accumulate as-is; no trimming happens below cap.
func TestStderrCap_SmallWritesAccumulate(t *testing.T) {
	sess := stderrTestSession(t)

	sess.appendStderr([]byte("first "))
	sess.appendStderr([]byte("second "))
	sess.appendStderr([]byte("third"))

	got := sess.StderrSnapshot()
	want := []byte("first second third")
	if !bytes.Equal(got, want) {
		t.Errorf("snapshot: want %q, got %q", want, got)
	}
}

// Combined write would exceed cap → front is trimmed; last stderrCap bytes
// preserved. Test against stderrCap directly so this stays correct if the
// constant is tuned.
func TestStderrCap_FrontTrimmedWhenExceeded(t *testing.T) {
	sess := stderrTestSession(t)

	// Fill to (cap - 5) with 'A's
	prefix := bytes.Repeat([]byte("A"), stderrCap-5)
	sess.appendStderr(prefix)
	if got := len(sess.StderrSnapshot()); got != stderrCap-5 {
		t.Fatalf("after prefix: want len=%d, got %d", stderrCap-5, got)
	}

	// Append 10 'B's — combined = cap+5 → expect trim of first 5 'A's,
	// resulting buffer = (cap-10) 'A's followed by 10 'B's.
	sess.appendStderr(bytes.Repeat([]byte("B"), 10))

	snap := sess.StderrSnapshot()
	if len(snap) != stderrCap {
		t.Errorf("buffer length must equal cap: want %d, got %d", stderrCap, len(snap))
	}
	wantAs := bytes.Repeat([]byte("A"), stderrCap-10)
	if !bytes.Equal(snap[:stderrCap-10], wantAs) {
		t.Errorf("expected (cap-10) leading A's; first byte=%c last leading byte=%c", snap[0], snap[stderrCap-11])
	}
	wantBs := bytes.Repeat([]byte("B"), 10)
	if !bytes.Equal(snap[stderrCap-10:], wantBs) {
		t.Errorf("expected 10 trailing B's; got %q", snap[stderrCap-10:])
	}
}

// Single write exactly equal to stderrCap → buffer becomes that write,
// nothing prior survives.
func TestStderrCap_SingleWriteEqualsCap(t *testing.T) {
	sess := stderrTestSession(t)

	sess.appendStderr([]byte("OLD-CONTENT"))
	bigP := bytes.Repeat([]byte("X"), stderrCap)
	sess.appendStderr(bigP)

	snap := sess.StderrSnapshot()
	if len(snap) != stderrCap {
		t.Errorf("len: want %d, got %d", stderrCap, len(snap))
	}
	if !bytes.Equal(snap, bigP) {
		t.Errorf("buffer should be exactly bigP after equal-cap write; first 5 bytes %q", snap[:5])
	}
}

// Single write > cap (the bug fixed in s3): only the LAST stderrCap bytes
// of the write are retained; buffer never exceeds cap. This case did not
// exercise via real drainStderr (4KB chunks) but must be correct per contract.
func TestStderrCap_SingleWriteExceedsCap_TailKept(t *testing.T) {
	sess := stderrTestSession(t)

	// Write cap+100 bytes: 100 'H' (head) followed by stderrCap 'T' (tail).
	huge := append(bytes.Repeat([]byte("H"), 100), bytes.Repeat([]byte("T"), stderrCap)...)
	sess.appendStderr(huge)

	snap := sess.StderrSnapshot()
	if len(snap) != stderrCap {
		t.Fatalf("buffer must equal cap, got %d (cap=%d)", len(snap), stderrCap)
	}
	wantTail := bytes.Repeat([]byte("T"), stderrCap)
	if !bytes.Equal(snap, wantTail) {
		t.Errorf("expected only the tail (all T's); first byte=%c last byte=%c", snap[0], snap[len(snap)-1])
	}
}

// Pathological: many tiny writes after the cap is reached — buffer must
// stay at exactly cap regardless of how many appends happen.
//
// This test only verifies the BOUND (the core invariant of §A.5), not the
// exact content of the tail. /bin/cat (used as a stub subprocess in this
// test setup) can emit a small amount of stderr if it gets unexpected
// argv from Spawn, and drainStderr would race with our appendStderr calls
// to interleave bytes. The other tests in this file (which run a fixed
// short sequence) prove tail-preservation correctness; this one is a
// bound stress-test only.
func TestStderrCap_RepeatedTinyWritesStayBounded(t *testing.T) {
	sess := stderrTestSession(t)

	// Saturate first.
	sess.appendStderr(bytes.Repeat([]byte("X"), stderrCap))
	for range 1000 {
		sess.appendStderr([]byte("y"))
	}
	snap := sess.StderrSnapshot()
	if len(snap) != stderrCap {
		t.Errorf("len after many tiny appends: want %d, got %d", stderrCap, len(snap))
	}
	// Verify our 'y's reached the back: the very last byte should be 'y'
	// (drainStderr from cat does not append at the end of an explicit
	// appendStderr call within the same goroutine flow).
	if last := snap[len(snap)-1]; last != 'y' {
		t.Errorf("last byte should be 'y' (most recent write), got %q", last)
	}
}
