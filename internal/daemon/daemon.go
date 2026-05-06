// Package daemon wires the v0.1 process skeleton from spec §4.1: the
// main goroutine constructs the watchdog and supervises two subsystem
// goroutines, the daemon (cc/PTY/lock) and the client (QUIC connection
// to server). v0.1 ships placeholder subsystems — the goal of this
// slice (Epic #3 #1) is the supervision skeleton, not the real work
// inside daemon/client.
//
// The cc PTY and other process-lifetime resources will be hoisted to
// main and threaded into the subsystem context in a follow-up slice
// (§4.2 ownership rules); for now the placeholder subsystems just run
// a heartbeat loop that proves the supervisor mechanics.
package daemon

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/piaobeizu/tether/internal/watchdog"
)

// Config bundles the runtime knobs that cmd/tether's flag parser
// supplies. Kept small and additive — feature flags grow this struct,
// not new constructor variants.
type Config struct {
	// Verbose enables supervisor + subsystem progress logging.
	Verbose bool

	// Stderr is where logger output lands. nil = io.Discard.
	Stderr io.Writer

	// HeartbeatTimeout overrides the watchdog default. Zero = use
	// watchdog.New() default (5s).
	HeartbeatTimeout time.Duration

	// SubsystemFactories lets tests install fake subsystems in place
	// of the real daemon/client. nil → use defaultSubsystems().
	SubsystemFactories []SubsystemFactory
}

// SubsystemFactory is a deferred constructor for a Subsystem. The
// supervisor calls the factory once on startup; the resulting
// Subsystem may be re-Run many times (each invocation = a fresh
// context, but the same instance — implementations must NOT keep
// per-run state on the receiver).
type SubsystemFactory func() watchdog.Subsystem

// Run constructs a Watchdog, registers the configured subsystems,
// and blocks until ctx is canceled. Returns nil on clean shutdown,
// non-nil only if the supervisor itself failed to start (today: never).
func Run(ctx context.Context, cfg Config) error {
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	wd := watchdog.New()
	if cfg.HeartbeatTimeout > 0 {
		wd.HeartbeatTimeout = cfg.HeartbeatTimeout
	}
	if cfg.Verbose {
		wd.Logger = func(format string, args ...any) {
			fmt.Fprintf(stderr, format+"\n", args...)
		}
	}

	factories := cfg.SubsystemFactories
	if factories == nil {
		factories = defaultSubsystems()
	}
	for _, f := range factories {
		wd.Supervise(f())
	}

	return wd.Run(ctx)
}

// defaultSubsystems is the v0.1 placeholder topology: a "daemon"
// subsystem (will own AgentProvider/PTY/lock in later slices) and a
// "client" subsystem (will hold the QUIC connection to server). Both
// currently just heartbeat — the watchdog is the deliverable here, not
// the workers.
func defaultSubsystems() []SubsystemFactory {
	return []SubsystemFactory{
		func() watchdog.Subsystem { return &placeholderSubsystem{name: "daemon", interval: time.Second} },
		func() watchdog.Subsystem { return &placeholderSubsystem{name: "client", interval: time.Second} },
	}
}

// placeholderSubsystem is the v0.1 skeleton-fill: a goroutine that
// beats every interval and otherwise does nothing. Real implementation
// arrives in subsequent Epic-3 slices (daemon: §5 AgentProvider
// orchestration; client: §3.3 wire connection to server).
type placeholderSubsystem struct {
	name     string
	interval time.Duration
}

func (p *placeholderSubsystem) Name() string { return p.name }

func (p *placeholderSubsystem) Run(ctx context.Context, hb func()) error {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	hb()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			hb()
		}
	}
}
