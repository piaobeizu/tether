package watchdog_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/watchdog"
)

// TestPoC3_Scenario3_OSSigkillSurrogate is the OS-level companion to
// TestPoC3_Scenario3_FatalErrorRestarted. It validates the spec §4.1
// "挡不住的" boundary — when an external SIGKILL targets a child
// PROCESS (not just a goroutine), the supervisor wrapping that
// process-level wait observes the abrupt exit and restarts.
//
// At the v0.1 daemon-skeleton layer there is no child process under
// the watchdog (subsystems are goroutines), so this test exercises an
// equivalent invariant: a Subsystem whose Run blocks on cmd.Wait()
// for a child shell process — and that child gets SIGKILL'd — exits
// with a fatal error which the supervisor classifies + restarts.
//
// This is the closest OS-level analog accessible without full
// process-tree restructuring (which arrives in a later slice when
// PTY ownership is hoisted out of the daemon goroutine per §4.2).
func TestPoC3_Scenario3_OSSigkillSurrogate(t *testing.T) {
	t.Parallel()

	logger := &testLogger{}
	wd := newFastWatchdog(logger)
	// SIGKILL takes ~immediate effect on Linux; allow the supervisor
	// to schedule a restart within 1s. Bump backoff a notch so the
	// restart isn't so fast it lands in the kill race.
	wd.BackoffInitial = 20 * time.Millisecond
	wd.BackoffMax = 100 * time.Millisecond

	// Locate /bin/sh once; if not present, this test is meaningless
	// (we'd be testing exec.LookPath, not the watchdog). Skip.
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("sh not available; SIGKILL surrogate test requires /bin/sh: %v", err)
	}

	var pidCh = make(chan int, 4)
	var killed atomic.Int64
	var clean atomic.Int64

	sub := watchdog.SubsystemFunc{
		N: "exec-runner",
		F: func(ctx context.Context, hb func() ) error {
			hb()
			// Spawn a child shell that just sleeps for a few seconds.
			// We deliberately don't pass ctx into the cmd so SIGKILL
			// from outside (the test) is the only way it dies; if
			// the supervisor cancels ctx (e.g. on parent shutdown),
			// we kill the child ourselves to drain.
			//
			//nolint:gosec // test code, fixed argv
			cmd := exec.Command(sh, "-c", "sleep 30")
			if err := cmd.Start(); err != nil {
				return err
			}
			select {
			case pidCh <- cmd.Process.Pid:
			default:
			}

			// Beat from a sibling goroutine so we keep the heartbeat
			// alive while cmd.Wait blocks.
			beatStop := make(chan struct{})
			watchdog.GoSafe(func() {
				t := time.NewTicker(30 * time.Millisecond)
				defer t.Stop()
				for {
					select {
					case <-beatStop:
						return
					case <-ctx.Done():
						return
					case <-t.C:
						hb()
					}
				}
			}, nil)

			// Cancel awareness: if ctx fires, kill the child.
			watchdog.GoSafe(func() {
				select {
				case <-ctx.Done():
					_ = cmd.Process.Kill()
				case <-beatStop:
				}
			}, nil)

			err := cmd.Wait()
			close(beatStop)
			if err == nil {
				clean.Add(1)
				return nil
			}
			// On SIGKILL, exec.ExitError reports signaled.
			if ee, ok := err.(*exec.ExitError); ok {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
					if ws.Signaled() && ws.Signal() == syscall.SIGKILL {
						killed.Add(1)
					}
				}
			}
			return err
		},
	}
	wd.Supervise(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	// Kill the first two children we spot; the third is allowed to live.
	killAttempts := 0
	deadline := time.Now().Add(3 * time.Second)
	for killAttempts < 2 && time.Now().Before(deadline) {
		select {
		case pid := <-pidCh:
			// SIGKILL the child shell.
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Logf("kill pid %d: %v (likely already gone)", pid, err)
			}
			killAttempts++
		case <-time.After(50 * time.Millisecond):
		}
	}

	waitFor(t, 5*time.Second, "supervisor to absorb 2 SIGKILLs and start a 3rd run", func() bool {
		return killed.Load() >= 2
	})

	// Drain remaining pidCh and stop.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog did not exit within 5s of cancel")
	}

	stats := wd.Stats()
	if stats[0].Restarts < 2 {
		t.Errorf("expected >=2 restarts after 2 SIGKILLs, got %d", stats[0].Restarts)
	}
	if stats[0].Panics > 0 {
		t.Errorf("SIGKILL surrogate must NOT count as panic; got %d", stats[0].Panics)
	}
	logs := logger.String()
	if !strings.Contains(logs, "exec-runner") {
		t.Errorf("expected supervisor logs to mention exec-runner, got:\n%s", logs)
	}

	// Sanity: confirm the test actually used a real binary on disk.
	if _, err := exec.LookPath(filepath.Base(sh)); err != nil {
		t.Errorf("post-condition: sh disappeared mid-test: %v", err)
	}
}
