package session

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// TestRegistry_EvictionOnContextCancel_Integration confirms the load-bearing
// tether#12 chain end-to-end against a REAL opencode process: the ctx passed
// to GetOrSpawnEntry bounds the agent subprocess (serveChat passes
// wtsess.Context()), so cancelling it — the daemon's "client disconnected"
// signal — closes the session's Events() channel, exits fanOut, and evicts the
// entry. The unit tests close Events() by hand; only a real provider exercises
// the ctx-cancel → closeEvents link that everything downstream depends on.
//
// No prompt is sent: spawning starts `opencode serve` and registers a pending
// entry, which is all we need to prove that a ctx cancel tears the entry back
// down. Gated behind TETHER_OPENCODE_IT and skipped when opencode is absent,
// mirroring the agent-package opencode integration test:
//
//	TETHER_OPENCODE_IT=1 GOWORK=off go test ./internal/session/ -run Integration -v
func TestRegistry_EvictionOnContextCancel_Integration(t *testing.T) {
	if os.Getenv("TETHER_OPENCODE_IT") == "" {
		t.Skip("set TETHER_OPENCODE_IT=1 to run the opencode integration test")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not found in PATH")
	}

	reg := NewRegistry(agent.NewOpenCodeProvider())

	ctx, cancel := context.WithCancel(context.Background())
	if _, err := reg.GetOrSpawnEntry(ctx, "", "opencode"); err != nil {
		cancel()
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	// Spawn registers the entry synchronously under its pending key; confirm
	// it is present before we tear it down.
	waitForCount(t, reg, 1)

	// Mimic the client disconnecting: cancel the connection context. This must
	// cascade through the real opencode session (closeEvents → fanOut exit →
	// evict). Poll with a generous bound to absorb real process teardown.
	cancel()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if regLen(reg) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("registry still holds %d entries 10s after ctx cancel, want 0", regLen(reg))
}
