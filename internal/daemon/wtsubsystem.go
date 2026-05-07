// wtsubsystem.go — watchdog-supervised owner of the WebTransport
// listener (slice #2 of the WT block).
//
// In slice #2 the wtSubsystem just accepts WT sessions and logs them.
// Slice #3 will rewire the session handler to dispatch envelopes
// off the events channel; for now the channel router is exercised
// only by tests, and the daemon exists primarily to prove the
// supervisor wiring.
//
// Restart safety: like clientSubsystem, this stores config rather
// than a built *wt.Server. Each Run() builds a fresh listener so a
// crashed Serve loop recovers cleanly on the next watchdog restart.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/piaobeizu/tether/internal/transport/wt"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// wtSubsystem owns the lifetime of the WT listener under the watchdog.
//
// It holds *config* (Addr + an optional pre-bound packet conn for
// tests), not a *wt.Server — so a watchdog restart constructs a fresh
// listener rather than re-running a closed one.
type wtSubsystem struct {
	addr string

	// listener is an optional pre-bound UDP socket for tests. Production
	// uses ListenAndServe via wt.Server.Serve; tests build a UDP socket
	// on :0 and pass it here so they can learn the bound port.
	listener net.PacketConn

	logf func(format string, args ...any)
	hb   func()
}

func (w *wtSubsystem) Name() string { return "wt" }

func (w *wtSubsystem) Run(ctx context.Context, hb func()) error {
	hb()

	srv, err := wt.New(ctx, wt.Config{
		Addr: w.addr,
		// Default session handler — sit on the session until close.
		// Slice #3 swaps this for envelope dispatch off the events
		// channel.
	})
	if err != nil {
		return fmt.Errorf("wt subsystem: new: %w", err)
	}
	defer func() { _ = srv.Close() }()

	// Heartbeat side goroutine so the supervisor's deadlock detector
	// stays happy while we just sit waiting for ctx / Serve to return.
	hbDone := make(chan struct{})
	defer func() { <-hbDone }()
	go func() {
		defer close(hbDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				hb()
			}
		}
	}()

	if w.logf != nil {
		w.logf("[daemon] wt listener starting on %s", w.addr)
	}

	var serveErr error
	if w.listener != nil {
		serveErr = srv.ServeListener(ctx, w.listener)
	} else {
		serveErr = srv.Serve(ctx)
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return serveErr
	}
	return nil
}

// wtSubsystemFactory wraps wtSubsystem under the SubsystemFactory
// signature daemon.Run consumes.
func wtSubsystemFactory(addr string, listener net.PacketConn, logf func(format string, args ...any)) SubsystemFactory {
	return func() watchdog.Subsystem {
		return &wtSubsystem{
			addr:     addr,
			listener: listener,
			logf:     logf,
		}
	}
}
