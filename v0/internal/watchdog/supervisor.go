package watchdog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Subsystem is one supervised goroutine (e.g. "daemon", "client" — see
// spec §4.1). It receives a per-run context and a Heartbeat func it
// MUST call regularly to prove liveness; missing the heartbeat for
// longer than the supervisor's HeartbeatTimeout is treated as a
// deadlock and the run is canceled + restarted.
//
// Run returns nil for clean exit (only when ctx is canceled — see
// resetSentinel below) or any error otherwise. Panics are caught by
// the supervisor's safeRun wrapper and surfaced as *PanicError, so
// implementations should not bother wrapping their own work in
// recover() — that's the supervisor's job.
type Subsystem interface {
	// Name is a stable string used in logs and metrics. Conventionally
	// short and lower-case ("daemon", "client").
	Name() string

	// Run is the worker loop. It MUST call hb() at least once every
	// HeartbeatTimeout/2 to avoid being killed for deadlock; the
	// supervisor's deadlock detector cancels the per-run ctx when the
	// heartbeat goes silent. Run should react by exiting promptly
	// (typically with ctx.Err()).
	//
	// Returning nil while ctx is NOT canceled is allowed but treated as
	// "subsystem voluntarily decided to stop" — supervisor still
	// restarts after backoff because v0.1 subsystems are expected to
	// run forever.
	Run(ctx context.Context, hb func()) error
}

// SubsystemFunc is an adapter so plain functions can satisfy Subsystem.
type SubsystemFunc struct {
	N string
	F func(ctx context.Context, hb func()) error
}

func (s SubsystemFunc) Name() string { return s.N }
func (s SubsystemFunc) Run(ctx context.Context, hb func()) error {
	return s.F(ctx, hb)
}

// Watchdog supervises one or more Subsystems. Construct with New, add
// subsystems with Supervise, then block in Run until the parent ctx is
// canceled. Restart counts and last errors are observable via Stats for
// tests and metrics.
type Watchdog struct {
	// HeartbeatTimeout bounds how long a subsystem may go without
	// calling hb() before the supervisor cancels its ctx. Zero =
	// disable deadlock detection (panic recovery + voluntary-error
	// restart still apply).
	HeartbeatTimeout time.Duration

	// HeartbeatCheckInterval is how often the supervisor wakes to
	// inspect the heartbeat. Smaller = faster detection, more CPU.
	// Default (when zero): HeartbeatTimeout / 4, clamped to [10ms, 1s].
	HeartbeatCheckInterval time.Duration

	// BackoffInitial / BackoffMax / BackoffFactor configure the restart
	// backoff. Zero values use NewBackoff defaults.
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	BackoffFactor  float64

	// HealthyRunWindow is the run duration after which the supervisor
	// resets the backoff schedule for that subsystem. Default = 30s.
	HealthyRunWindow time.Duration

	// Logger is called once per supervisory event (start, error,
	// heartbeat-timeout, restart). nil = silent (handy for tests).
	// The supervisor serializes all calls through logMu — implementers
	// may use a non-thread-safe sink (e.g. bytes.Buffer) without
	// further locking.
	Logger func(format string, args ...any)

	mu         sync.Mutex
	subsystems []*supervised
	wg         sync.WaitGroup
	started    bool

	logMu sync.Mutex
}

// supervised is the per-subsystem state the watchdog tracks.
type supervised struct {
	sub Subsystem

	restartCount atomic.Int64
	panicCount   atomic.Int64
	timeoutCount atomic.Int64

	mu      sync.Mutex
	lastErr error
}

// SubsystemStats is a snapshot for tests / observability.
type SubsystemStats struct {
	Name         string
	Restarts     int64
	Panics       int64
	Timeouts     int64
	LastErr      error
}

// New constructs a Watchdog with sensible defaults. Callers may further
// tune fields before calling Supervise / Run.
func New() *Watchdog {
	return &Watchdog{
		HeartbeatTimeout:       5 * time.Second,
		HeartbeatCheckInterval: 0, // auto
		BackoffInitial:         200 * time.Millisecond,
		BackoffMax:             10 * time.Second,
		BackoffFactor:          2,
		HealthyRunWindow:       30 * time.Second,
	}
}

// Supervise registers a Subsystem. Must be called before Run; calling
// after Run starts is a panic — supervisor topology is fixed for the
// lifetime of the watchdog (matches spec §4.1: there are exactly two
// subsystems, daemon + client).
func (w *Watchdog) Supervise(sub Subsystem) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		panic("watchdog: Supervise after Run is forbidden")
	}
	w.subsystems = append(w.subsystems, &supervised{sub: sub})
}

// Run starts a superviseLoop per registered subsystem and blocks until
// ctx is canceled. After ctx cancels, Run waits for all loops to finish
// then returns nil. Run is single-shot — calling twice is a programming
// error and panics.
func (w *Watchdog) Run(ctx context.Context) error {
	w.mu.Lock()
	if w.started {
		w.mu.Unlock()
		panic("watchdog: Run called twice")
	}
	w.started = true
	subs := append([]*supervised(nil), w.subsystems...)
	w.mu.Unlock()

	for _, s := range subs {
		w.wg.Add(1)
		s := s
		// Supervisor loops are themselves goroutines, but they're
		// tracked by w.wg and re-entered through safeRun in their
		// inner loop, so naked `go` is acceptable here. Subsystem
		// implementations must use GoSafe for any further fan-out.
		go func() {
			defer w.wg.Done()
			w.superviseLoop(ctx, s)
		}()
	}

	<-ctx.Done()
	w.wg.Wait()
	return nil
}

// Stats returns a snapshot of all subsystems' counters.
func (w *Watchdog) Stats() []SubsystemStats {
	w.mu.Lock()
	subs := append([]*supervised(nil), w.subsystems...)
	w.mu.Unlock()

	out := make([]SubsystemStats, 0, len(subs))
	for _, s := range subs {
		s.mu.Lock()
		lastErr := s.lastErr
		s.mu.Unlock()
		out = append(out, SubsystemStats{
			Name:     s.sub.Name(),
			Restarts: s.restartCount.Load(),
			Panics:   s.panicCount.Load(),
			Timeouts: s.timeoutCount.Load(),
			LastErr:  lastErr,
		})
	}
	return out
}

// errHeartbeatTimeout is the supervisor-internal sentinel returned when
// the deadlock detector kills a run. Surfaced through Stats.LastErr so
// callers can match with errors.Is.
var errHeartbeatTimeout = errors.New("watchdog: subsystem heartbeat timeout (deadlock)")

// ErrHeartbeatTimeout is the public alias for errors.Is comparisons.
var ErrHeartbeatTimeout = errHeartbeatTimeout

// superviseLoop runs one supervised subsystem forever (until ctx is
// canceled). Each iteration:
//
//  1. derives a per-run cancellable ctx
//  2. starts a heartbeat watcher in a GoSafe goroutine
//  3. invokes safeRun(s.sub.Run)
//  4. tears down the heartbeat watcher
//  5. on parent-ctx cancel → exit; otherwise sleep backoff, then loop
//
// State machine summary:
//
//	[idle] → start → [running]
//	[running] → fn returns nil → [stopped]      (only valid when ctx done)
//	[running] → fn returns err  → [crashed]
//	[running] → fn panics       → [crashed]
//	[running] → hb timeout      → [crashed]     (per-run ctx canceled, fn drained, errHeartbeatTimeout recorded)
//	[crashed] → backoff.Next()  → [restarting] → [running]   (loop)
//	[stopped]                                                  (exit superviseLoop)
func (w *Watchdog) superviseLoop(parent context.Context, s *supervised) {
	backoff := NewBackoff(w.BackoffInitial, w.BackoffMax, w.BackoffFactor)
	healthyWindow := w.HealthyRunWindow
	if healthyWindow <= 0 {
		healthyWindow = 30 * time.Second
	}

	for {
		if parent.Err() != nil {
			return
		}
		runCtx, runCancel := context.WithCancel(parent)
		startedAt := time.Now()

		hb := newHeartbeat()
		hbDone := make(chan struct{})
		var hbTimedOut atomic.Bool

		// Heartbeat watcher: cancels runCtx if hb goes silent for too
		// long. Uses GoSafe to satisfy §4.3 — even our own internal
		// goroutines route through the panic-recovery helper.
		if w.HeartbeatTimeout > 0 {
			GoSafe(func() {
				defer close(hbDone)
				w.watchHeartbeat(runCtx, hb, runCancel, &hbTimedOut)
			}, func(pe *PanicError) {
				// Heartbeat watcher panic is itself a bug; record it
				// but don't try to "supervise the supervisor" — exit
				// the watcher and let the run continue without
				// deadlock detection (heartbeat is a defense-in-depth
				// mechanism, not the only one).
				w.logf("[watchdog] %s: heartbeat watcher panicked: %v", s.sub.Name(), pe)
				close(hbDone)
			})
		} else {
			close(hbDone)
		}

		w.logf("[watchdog] %s: starting (run #%d)", s.sub.Name(), s.restartCount.Load()+1)
		err := safeRun(runCtx, func(ctx context.Context) error {
			return s.sub.Run(ctx, hb.Beat)
		})
		runCancel()
		<-hbDone

		// Translate a heartbeat-induced cancellation into the dedicated
		// sentinel so observers can distinguish "deadlock kill" from
		// "ordinary error".
		if hbTimedOut.Load() {
			err = errHeartbeatTimeout
			s.timeoutCount.Add(1)
		}

		if IsPanic(err) {
			s.panicCount.Add(1)
		}

		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()

		// Parent-cancel takeover: if the parent ctx is done, exit
		// regardless of how the run ended.
		if parent.Err() != nil {
			if err != nil {
				w.logf("[watchdog] %s: shutting down with last err: %v", s.sub.Name(), summarize(err))
			} else {
				w.logf("[watchdog] %s: shutting down cleanly", s.sub.Name())
			}
			return
		}

		// Reset backoff if the run lasted long enough to count as
		// healthy — prevents transient panics from permanently
		// inflating the delay.
		if time.Since(startedAt) >= healthyWindow {
			backoff.Reset()
		}
		delay := backoff.Next()
		s.restartCount.Add(1)
		w.logf("[watchdog] %s: failed (%v); restarting in %v",
			s.sub.Name(), summarize(err), delay)

		select {
		case <-parent.Done():
			return
		case <-time.After(delay):
		}
	}
}

func (w *Watchdog) watchHeartbeat(
	ctx context.Context,
	hb *heartbeat,
	cancel context.CancelFunc,
	timedOut *atomic.Bool,
) {
	interval := w.HeartbeatCheckInterval
	if interval <= 0 {
		interval = w.HeartbeatTimeout / 4
		if interval < 10*time.Millisecond {
			interval = 10 * time.Millisecond
		}
		if interval > time.Second {
			interval = time.Second
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			last := hb.LastBeat()
			if last.IsZero() {
				// Subsystem hasn't beat once yet — give it the full
				// timeout from start; tracked by ctx start time
				// approximated via heartbeat creation.
				if now.Sub(hb.createdAt) > w.HeartbeatTimeout {
					timedOut.Store(true)
					cancel()
					return
				}
				continue
			}
			if now.Sub(last) > w.HeartbeatTimeout {
				timedOut.Store(true)
				cancel()
				return
			}
		}
	}
}

func (w *Watchdog) logf(format string, args ...any) {
	if w.Logger == nil {
		return
	}
	w.logMu.Lock()
	defer w.logMu.Unlock()
	w.Logger(format, args...)
}

// summarize keeps log lines short — full panic stacks blow out logs.
func summarize(err error) string {
	if err == nil {
		return "<nil>"
	}
	s := err.Error()
	const cap = 240
	if len(s) > cap {
		return fmt.Sprintf("%s... [truncated]", s[:cap])
	}
	return s
}
