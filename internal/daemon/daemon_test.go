package daemon_test

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/daemon"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// TestRun_StartsAndDrainsOnCancel — minimum smoke test: calling
// Run with default subsystems should boot, supervise, and exit cleanly
// when ctx cancels.
func TestRun_StartsAndDrainsOnCancel(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	cfg := daemon.Config{
		Verbose: true,
		Stderr:  &stderr,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Let it run a moment so we see "starting" log lines for both
	// subsystems.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon.Run did not exit within 2s of ctx cancel")
	}

	logs := stderr.String()
	for _, want := range []string{"daemon", "client", "starting"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected logs to mention %q; got:\n%s", want, logs)
		}
	}
}

// TestRun_CustomFactory — a Config-injected factory is honored
// (this is the seam tests + later real implementations use to wire
// the actual daemon/client subsystems).
func TestRun_CustomFactory(t *testing.T) {
	t.Parallel()

	var beats atomic.Int64
	factory := func() watchdog.Subsystem {
		return watchdog.SubsystemFunc{
			N: "test-sub",
			F: func(ctx context.Context, hb func()) error {
				t := time.NewTicker(10 * time.Millisecond)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-t.C:
						hb()
						beats.Add(1)
					}
				}
			},
		}
	}

	cfg := daemon.Config{
		HeartbeatTimeout:   100 * time.Millisecond,
		SubsystemFactories: []daemon.SubsystemFactory{factory},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Wait for a few beats.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && beats.Load() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	if beats.Load() < 5 {
		t.Errorf("expected >=5 beats from custom subsystem; got %d", beats.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon.Run did not exit within 2s of ctx cancel")
	}
}
