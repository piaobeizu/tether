package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/lock"
)

// TestClientSubsystem_Run_IsRestartable verifies that calling Run on
// the SAME *clientSubsystem twice in sequence (the supervisor's restart
// pattern) succeeds both times — i.e. the second Run rebuilds the
// AttachServer listener instead of trying to reuse a closed one.
//
// Regression: the previous implementation captured a pre-built
// *agent.AttachServer on the receiver. Once Run returned, the listener
// was closed; restart re-Ran the same instance, Serve fell through on
// the closed listener, and the daemon silently became attach-disabled
// while still beating heartbeats.
func TestClientSubsystem_Run_IsRestartable(t *testing.T) {
	tmp := t.TempDir()
	socketPath := filepath.Join(tmp, "attach.sock")
	projectsDir := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	emCtx, emCancel := context.WithCancel(context.Background())
	defer emCancel()
	em, err := agent.NewEnvelopeEmitter(emCtx, agent.EmitterConfig{
		ProjectsDir: projectsDir,
	})
	if err != nil {
		t.Fatalf("emitter: %v", err)
	}
	defer em.Close()
	lk := lock.New()

	c := &clientSubsystem{
		socketPath: socketPath,
		emitter:    em,
		lock:       lk,
	}

	runOnce := func(label string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hbCount := 0
		hb := func() { hbCount++ }
		errCh := make(chan error, 1)
		go func() { errCh <- c.Run(ctx, hb) }()

		// Wait for the socket to bind.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(socketPath); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := os.Stat(socketPath); err != nil {
			t.Fatalf("[%s] socket never bound: %v", label, err)
		}

		// Sanity dial — proves the listener is live, not just the
		// socket file.
		conn, err := net.DialTimeout("unix", socketPath, time.Second)
		if err != nil {
			t.Fatalf("[%s] dial: %v", label, err)
		}
		_ = conn.Close()

		cancel()
		select {
		case err := <-errCh:
			if err != nil && err != context.Canceled {
				t.Fatalf("[%s] Run returned %v", label, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("[%s] Run did not exit within 2s of cancel", label)
		}
		if hbCount == 0 {
			t.Errorf("[%s] heartbeat never fired", label)
		}
	}

	runOnce("first")
	// CRITICAL: same receiver, second invocation. Before the fix this
	// would fail because the captured *AttachServer was already closed
	// (or os.Remove of the bind path on the second NewAttachServer
	// call would have raced with the kernel's lingering bind).
	runOnce("second-restart")
	runOnce("third-restart")
}
