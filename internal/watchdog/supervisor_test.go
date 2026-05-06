package watchdog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/watchdog"
)

// testLogger captures supervisor log lines for assertions.
type testLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *testLogger) Logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *testLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// scriptedSubsystem is a Subsystem whose Run delegates to a function
// the test supplies. Tests can flip behavior between runs by mutating
// the closure's captured state.
type scriptedSubsystem struct {
	name string
	fn   func(ctx context.Context, hb func(), runIdx int) error

	mu     sync.Mutex
	runIdx int
}

func (s *scriptedSubsystem) Name() string { return s.name }

func (s *scriptedSubsystem) Run(ctx context.Context, hb func()) error {
	s.mu.Lock()
	idx := s.runIdx
	s.runIdx++
	s.mu.Unlock()
	return s.fn(ctx, hb, idx)
}

func (s *scriptedSubsystem) Runs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runIdx
}

// newFastWatchdog returns a watchdog tuned for tests: very fast
// heartbeat detection + minimal backoff so a 3-restart sequence
// completes in a fraction of a second.
func newFastWatchdog(logger *testLogger) *watchdog.Watchdog {
	wd := watchdog.New()
	wd.HeartbeatTimeout = 100 * time.Millisecond
	wd.HeartbeatCheckInterval = 20 * time.Millisecond
	wd.BackoffInitial = 5 * time.Millisecond
	wd.BackoffMax = 50 * time.Millisecond
	wd.HealthyRunWindow = 200 * time.Millisecond
	if logger != nil {
		wd.Logger = logger.Logf
	}
	return wd
}

// waitFor polls until cond returns true or timeout expires; reports
// failure with msg when it times out.
func waitFor(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitFor: timeout (%v): %s", timeout, msg)
}

// ------------------------------------------------------------------
// Scenario 1 — sub-goroutine panics, supervisor recovers + restarts.
//
// PoC-3 #1: "注入 panic 到 daemon goroutine, cc 是否存活、新 daemon
// goroutine 是否正常接管".
// ------------------------------------------------------------------
func TestPoC3_Scenario1_PanicInSubsystemRestarts(t *testing.T) {
	t.Parallel()

	logger := &testLogger{}
	wd := newFastWatchdog(logger)

	const wantRuns = 3 // first run panics; expect supervisor to restart at least twice more.
	sub := &scriptedSubsystem{
		name: "daemon",
		fn: func(ctx context.Context, hb func(), runIdx int) error {
			hb()
			if runIdx < 2 {
				// First two runs panic — exercises the panic-recover path.
				// Different shapes: first nil-deref-style, second string-panic.
				if runIdx == 0 {
					var p *int
					_ = *p // nil deref panic
				}
				panic("scripted panic for PoC-3 #1")
			}
			// Third run: settle and just heartbeat until the test
			// cancels the parent ctx.
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					hb()
				}
			}
		},
	}
	wd.Supervise(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	waitFor(t, 2*time.Second, "subsystem to settle on run 3 (after 2 panics)", func() bool {
		return sub.Runs() >= wantRuns
	})

	stats := wd.Stats()
	if len(stats) != 1 {
		t.Fatalf("expected 1 subsystem in stats, got %d", len(stats))
	}
	if stats[0].Panics < 2 {
		t.Errorf("expected >=2 recovered panics, got %d", stats[0].Panics)
	}
	if stats[0].Restarts < 2 {
		t.Errorf("expected >=2 restarts, got %d", stats[0].Restarts)
	}

	// Tear down.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return after parent cancel")
	}

	if !strings.Contains(logger.String(), "panic:") {
		t.Errorf("expected supervisor log to mention panic, got:\n%s", logger.String())
	}
}

// ------------------------------------------------------------------
// Scenario 2 — sub-goroutine deadlocks (sleeps forever, ignores ctx
// AND stops heartbeating) → watchdog detects via heartbeat timeout,
// cancels run ctx, and restarts.
//
// PoC-3 #2: "注入死循环到 daemon, watchdog 是否在 10s 内发现 + cancel
// ctx + 重启". The integration uses a 100ms heartbeat budget so the
// test runs in well under a second; the spec's 10s budget is the
// production knob.
// ------------------------------------------------------------------
func TestPoC3_Scenario2_DeadlockDetectedAndRestarted(t *testing.T) {
	t.Parallel()

	logger := &testLogger{}
	wd := newFastWatchdog(logger)

	// We deliberately want a subsystem that reaches a deadlock: it
	// beats once to prove it started, then blocks on a select with
	// a never-receiving channel AND no <-ctx.Done() arm. The only
	// thing that can free it is the goroutine being abandoned when
	// the supervisor decides to give up (we don't actually leak —
	// we'll close stuck after the test to release the goroutine).
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	sub := &scriptedSubsystem{
		name: "daemon",
		fn: func(ctx context.Context, hb func(), runIdx int) error {
			hb()
			if runIdx == 0 {
				// Honor ctx so we exit promptly when the supervisor
				// cancels — but DON'T heartbeat anymore. Watchdog
				// must rely on the heartbeat alone (this is the
				// "deadlock-shaped" behavior: goroutine is alive
				// but stuck not making progress).
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-stuck:
					return nil
				}
			}
			// Subsequent runs: heartbeat normally.
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					hb()
				}
			}
		},
	}
	wd.Supervise(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	// Wait for at least one timeout-induced restart + a healthy run.
	waitFor(t, 2*time.Second, "supervisor to detect deadlock and start a healthy run", func() bool {
		stats := wd.Stats()
		return len(stats) == 1 &&
			stats[0].Timeouts >= 1 &&
			sub.Runs() >= 2
	})

	stats := wd.Stats()
	if !errors.Is(stats[0].LastErr, watchdog.ErrHeartbeatTimeout) && stats[0].Timeouts < 1 {
		t.Errorf("expected at least one heartbeat timeout, got Timeouts=%d LastErr=%v",
			stats[0].Timeouts, stats[0].LastErr)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return after parent cancel")
	}

	if !strings.Contains(logger.String(), "heartbeat timeout") {
		t.Errorf("expected supervisor log to mention heartbeat timeout, got:\n%s", logger.String())
	}
}

// ------------------------------------------------------------------
// Scenario 3 — sub-goroutine "killed externally" (proxy for the
// SIGKILL case in PoC-3 #3). At goroutine granularity, "external
// SIGKILL" is simulated as: the subsystem returns a fatal error that
// the supervisor must treat as restart-worthy (it does NOT take down
// the process — only main can os.Exit per §4.3).
//
// PoC-3 #3 spec text says "main 进程被 SIGKILL，下次启动 needs-resume
// 流程是否正常" — the resume flow is a separate slice (§6.2).  In
// THIS slice we cover the analogous goroutine-level invariant:
// abrupt-exit subsystem is restarted, supervisor logs, accounting is
// correct.  An end-to-end OS SIGKILL test against a child binary lives
// alongside in the cmd-level integration test below.
// ------------------------------------------------------------------
func TestPoC3_Scenario3_FatalErrorRestarted(t *testing.T) {
	t.Parallel()

	logger := &testLogger{}
	wd := newFastWatchdog(logger)

	var fatalCount atomic.Int64
	sub := &scriptedSubsystem{
		name: "daemon",
		fn: func(ctx context.Context, hb func(), runIdx int) error {
			hb()
			if runIdx < 2 {
				fatalCount.Add(1)
				return errors.New("subsystem killed externally (simulated SIGKILL)")
			}
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					hb()
				}
			}
		},
	}
	wd.Supervise(sub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	waitFor(t, 2*time.Second, "supervisor to absorb 2 fatal exits", func() bool {
		return fatalCount.Load() >= 2 && sub.Runs() >= 3
	})

	stats := wd.Stats()
	if stats[0].Restarts < 2 {
		t.Errorf("expected >=2 restarts, got %d", stats[0].Restarts)
	}
	if stats[0].Panics > 0 {
		t.Errorf("fatal-error path must NOT count as panic; got Panics=%d", stats[0].Panics)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not return after parent cancel")
	}

	if !strings.Contains(logger.String(), "killed externally") {
		t.Errorf("expected supervisor log to surface fatal error, got:\n%s", logger.String())
	}
}

// ------------------------------------------------------------------
// Sanity tests for the supervisor mechanics that PoC-3 leans on.
// ------------------------------------------------------------------

// TestSupervise_SiblingIsolation — when one supervised subsystem
// panics repeatedly, the other one continues running undisturbed.
// This is the §4.1 invariant: a daemon panic must not affect the
// client subsystem and vice versa.
func TestSupervise_SiblingIsolation(t *testing.T) {
	t.Parallel()

	wd := newFastWatchdog(nil)

	flaky := &scriptedSubsystem{
		name: "flaky",
		fn: func(ctx context.Context, hb func(), runIdx int) error {
			hb()
			if runIdx < 3 {
				panic("flaky run")
			}
			t := time.NewTicker(20 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					hb()
				}
			}
		},
	}

	var stableBeats atomic.Int64
	stable := &scriptedSubsystem{
		name: "stable",
		fn: func(ctx context.Context, hb func(), _ int) error {
			t := time.NewTicker(10 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-t.C:
					hb()
					stableBeats.Add(1)
				}
			}
		},
	}

	wd.Supervise(flaky)
	wd.Supervise(stable)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	waitFor(t, 2*time.Second, "flaky to settle and stable to keep beating", func() bool {
		return flaky.Runs() >= 4 && stableBeats.Load() >= 5 && stable.Runs() == 1
	})

	if stable.Runs() != 1 {
		t.Errorf("stable subsystem must NOT have been restarted by sibling panics; got Runs=%d", stable.Runs())
	}

	cancel()
	<-done
}

// TestSupervise_ParentCancelDrains — once the parent ctx cancels, all
// supervised subsystems must exit promptly and Run must return.
func TestSupervise_ParentCancelDrains(t *testing.T) {
	t.Parallel()

	wd := newFastWatchdog(nil)

	var sawCancel atomic.Bool
	sub := &scriptedSubsystem{
		name: "daemon",
		fn: func(ctx context.Context, hb func(), _ int) error {
			t := time.NewTicker(10 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					sawCancel.Store(true)
					return ctx.Err()
				case <-t.C:
					hb()
				}
			}
		},
	}
	wd.Supervise(sub)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wd.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned non-nil error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not exit within 2s of parent cancel")
	}
	if !sawCancel.Load() {
		t.Error("subsystem did not observe ctx cancellation")
	}
}

// TestSupervise_DoubleRunPanics — Run is single-shot (defensive).
func TestSupervise_DoubleRunPanics(t *testing.T) {
	t.Parallel()

	wd := newFastWatchdog(nil)
	wd.Supervise(&scriptedSubsystem{
		name: "noop",
		fn: func(ctx context.Context, hb func(), _ int) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = wd.Run(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)

	defer func() {
		if r := recover(); r == nil {
			t.Error("expected second Run to panic")
		}
		cancel()
		<-done
	}()
	_ = wd.Run(ctx)
}

// TestSupervise_AfterStartIsRefused — Supervise after Run starts is a
// hard error (topology is fixed for the watchdog's lifetime).
func TestSupervise_AfterStartIsRefused(t *testing.T) {
	t.Parallel()

	wd := newFastWatchdog(nil)
	wd.Supervise(&scriptedSubsystem{
		name: "x",
		fn: func(ctx context.Context, hb func(), _ int) error {
			<-ctx.Done()
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = wd.Run(ctx)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)

	defer func() {
		if r := recover(); r == nil {
			t.Error("Supervise after Run should panic")
		}
		cancel()
		<-done
	}()
	wd.Supervise(&scriptedSubsystem{name: "late"})
}
