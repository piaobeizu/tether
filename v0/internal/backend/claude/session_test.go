package claude

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// requireClaude skips a test when the claude binary is not on PATH.
func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not in PATH; skipping integration test")
	}
}

// TestSession_NewAndClose covers the basic happy-path lifecycle with real
// claude: Spawn → Start(initial prompt) → init arrives → SessionID
// populated → Close. (claude does not emit system/init until it has read
// the first user message on stdin — see New godoc.)
func TestSession_NewAndClose_RealClaude(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{Model: "haiku"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Drain events so the dispatcher doesn't block on a full chan.
	go func() {
		for range sess.Events() {
		}
	}()

	if err := sess.Start(ctx, "Reply with just 'ok'."); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if sess.SessionID() == "" {
		t.Error("SessionID must be non-empty after Start")
	}

	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSession_SendAndResult covers spike scenario 1 (full conversation):
// Send a prompt → drain events until result → assert no error.
func TestSession_SendAndResult_RealClaude(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{Model: "haiku"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sess.Close()

	// Drain events in a goroutine — collect until result, then signal.
	type endResult struct {
		isError bool
		stop    string
	}
	doneCh := make(chan endResult, 1)
	go func() {
		for env := range sess.Events() {
			if env.Type == EventResult {
				r, err := env.DecodeResult()
				if err != nil {
					t.Errorf("DecodeResult: %v", err)
					return
				}
				doneCh <- endResult{isError: r.IsError, stop: r.StopReason}
				return
			}
		}
		doneCh <- endResult{} // channel closed without result
	}()

	if err := sess.Start(ctx, "Reply with just the word 'ok'."); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case r := <-doneCh:
		if r.isError {
			t.Errorf("result was an error (stop=%s)", r.stop)
		}
		t.Logf("scenario 1 PASS — stop_reason=%s", r.stop)
	case <-time.After(45 * time.Second):
		t.Fatal("did not receive result within 45s")
	}
}

// TestSession_InitTimeout verifies spec §6.A.4: when the subprocess never
// emits system/init after we Start it, the call must return ErrInitTimeout
// — not hang forever.
//
// We exercise this by pointing Spawn at /bin/cat which echoes stdin to
// stdout; our user envelope shows up on stdout but isn't a valid system/init,
// so the wait times out.
func TestSession_InitTimeout(t *testing.T) {
	if _, err := exec.LookPath("/bin/cat"); err != nil {
		t.Skip("/bin/cat not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{BinaryPath: "/bin/cat"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sess.Close()

	// Drain events so dispatch can run.
	go func() {
		for range sess.Events() {
		}
	}()

	start := time.Now()
	err = sess.startWithInitTimeout(ctx, "ping", 300*time.Millisecond)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrInitTimeout) {
		t.Errorf("expected ErrInitTimeout, got: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Start took too long: %v (expected ~300ms)", elapsed)
	}
}

// TestSession_SendAfterKill verifies spec §6.A.3: when the subprocess has
// died (we SIGKILL it), the next Send must return ErrSubprocessGone — not
// a generic syscall.EPIPE wrapped error.
func TestSession_SendAfterKill_RealClaude(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{Model: "haiku"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer sess.Close()

	go func() {
		for range sess.Events() {
		}
	}()

	if err := sess.Start(ctx, "say ok"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Kill the underlying process directly — Session itself doesn't know.
	if err := sess.sub.Cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Give the kernel a beat to tear down the pipe.
	time.Sleep(200 * time.Millisecond)

	err = sess.Send("hello?")
	if !errors.Is(err, ErrSubprocessGone) {
		t.Errorf("expected ErrSubprocessGone, got: %v", err)
	}
}

// TestSession_StderrDrain_NeverBlocks is a smoke test for spec §6.A.5.
// We can't easily flood stderr with a real claude invocation (cc emits
// little stderr), so we instead verify (a) the StderrSnapshot path works
// and (b) the drain goroutine exits cleanly on Close.
func TestSession_StderrDrain_RealClaude(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{Model: "haiku"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	go func() {
		for range sess.Events() {
		}
	}()

	// Trivial query just to produce some activity.
	if err := sess.Start(ctx, "say ok"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(2 * time.Second)

	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// After Close, the drain goroutine should have exited and Snapshot
	// should be safe.
	snap := sess.StderrSnapshot()
	t.Logf("stderr snapshot: %d bytes", len(snap))

	// Calling Close again should be safe + return same nil error.
	if err := sess.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSession_Close_Concurrent ensures Close() is safe to invoke from
// multiple goroutines simultaneously — the underlying Wait runs once.
func TestSession_Close_Concurrent_RealClaude(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sess, err := New(ctx, SpawnOpts{Model: "haiku"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go func() {
		for range sess.Events() {
		}
	}()
	if err := sess.Start(ctx, "say ok"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const N = 5
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sess.Close()
		}()
	}
	wg.Wait()

	// Sanity: after concurrent Close storm, Send must return
	// ErrSessionClosed (not ErrSubprocessGone — closed flag wins).
	err = sess.Send("late")
	if !errors.Is(err, ErrSessionClosed) {
		t.Errorf("expected ErrSessionClosed, got: %v", err)
	}
}
