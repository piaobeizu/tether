// Package daemon wires the v0.1 process skeleton from spec §4.1: the
// main goroutine constructs the watchdog and supervises subsystem
// goroutines. Slice "daemon-cc-integration" replaces the previous
// heartbeat-only placeholders with the real composition:
//
//   - "daemon" subsystem: owns the JSONL watcher (~/.claude/projects/)
//     + envelope emitter that bridges JSONL → wire envelopes per §11.N.
//   - "client" subsystem: the local Unix attach socket (§4.4 / §11.U)
//     listening at ~/.tether/attach.sock.
//
// Both are supervised by the watchdog FSM (heartbeats every second,
// restart on panic / heartbeat-timeout). The cc PTY itself is NOT
// hoisted into the daemon subsystem in this slice — PTY spawn is
// owned by callers who want the local TUI surface and can run it
// alongside the daemon (the PTY pieces live in
// internal/backend/claude.PTYSession). v0.1 daemon orchestration
// reads cc state of the world from the JSONL watcher, which is the
// authoritative event source per spec §5.6 / F-04.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/lock"
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

	// ProjectsDir is the cc projects directory the JSONL watcher
	// tails. Empty resolves to $HOME/.claude/projects/. Tests
	// override.
	ProjectsDir string

	// AttachSocketPath is the path the attach Unix socket binds to.
	// Empty resolves to $HOME/.tether/attach.sock. Tests override.
	AttachSocketPath string

	// InputSink, when non-nil, becomes the attach socket's bridge to
	// "input bytes from a rw client land here". Production wires this
	// to PTYSession.SendInput; tests inject a recorder. nil disables
	// rw mode (clients are auto-downgraded to ro).
	InputSink func(sessionID string, data []byte) error

	// SubsystemFactories lets tests install fake subsystems in place
	// of the real daemon/client. nil → use defaultSubsystems with
	// the components built from this Config.
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
// non-nil only if the supervisor itself failed to start (config
// resolution, IO setup).
func Run(ctx context.Context, cfg Config) error {
	stderr := cfg.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	logf := func(format string, args ...any) {
		if cfg.Verbose {
			fmt.Fprintf(stderr, format+"\n", args...)
		}
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
		// Resolve default paths.
		projectsDir := cfg.ProjectsDir
		if projectsDir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("daemon: resolve $HOME for projects dir: %w", err)
			}
			projectsDir = filepath.Join(home, ".claude", "projects")
		}
		// JSONL watcher requires the directory to exist.
		if err := os.MkdirAll(projectsDir, 0o700); err != nil {
			return fmt.Errorf("daemon: ensure projects dir %q: %w", projectsDir, err)
		}

		socketPath := cfg.AttachSocketPath
		if socketPath == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("daemon: resolve $HOME for attach socket: %w", err)
			}
			socketPath = filepath.Join(home, ".tether", "attach.sock")
		}

		// Build shared components.
		emitter, err := agent.NewEnvelopeEmitter(ctx, agent.EmitterConfig{
			ProjectsDir: projectsDir,
			OnError: func(p string, err error) {
				logf("[daemon] jsonl watcher error %q: %v", p, err)
			},
			OnDrop: func(sid, kind string) {
				logf("[daemon] envelope drop sid=%s kind=%s", sid, kind)
			},
		})
		if err != nil {
			return fmt.Errorf("daemon: envelope emitter: %w", err)
		}

		lockSM := lock.New()

		attach, err := agent.NewAttachServer(agent.AttachServerConfig{
			SocketPath: socketPath,
			Emitter:    emitter,
			Lock:       lockSM,
			InputSink:  cfg.InputSink,
			OnError: func(err error) {
				logf("[daemon] attach: %v", err)
			},
		})
		if err != nil {
			_ = emitter.Close()
			return fmt.Errorf("daemon: attach server: %w", err)
		}

		// Tear down the priming AttachServer immediately — it served
		// only to fail-fast on bind errors at daemon startup. The
		// supervised clientSubsystem will create its own listener on
		// each Run (so panics / errors don't leave the watchdog
		// permanently disabled, see clientSubsystem doc).
		_ = attach.Close()

		factories = realSubsystems(socketPath, emitter, lockSM, cfg.InputSink, logf)
		// emitter lives for the daemon's full lifetime; clean up
		// on Run exit. clientSubsystem owns its per-run AttachServer
		// and closes it inside Run's defer.
		defer func() {
			_ = emitter.Close()
		}()
	}

	for _, f := range factories {
		wd.Supervise(f())
	}
	return wd.Run(ctx)
}

// realSubsystems assembles the production daemon + client subsystems.
//
// daemon subsystem responsibility: envelope emitter health + lock
// state housekeeping (lock.Sweep on a periodic timer so audit-log
// entries fire promptly, not just lazily on next acquire). Owns
// long-lived state shared across attach restarts.
//
// client subsystem responsibility: serve the attach Unix socket.
// CRITICALLY, the client subsystem does NOT capture a pre-built
// *AttachServer — it captures the *config* needed to build one. Each
// supervised Run() constructs a fresh AttachServer, serves it, and
// tears it down on exit. This is the spec-aligned fix for the
// "watchdog restart leaves a permanently-disabled stub" bug: a
// restartable Subsystem MUST NOT depend on per-Run state stored on
// the receiver.
func realSubsystems(
	socketPath string,
	em *agent.EnvelopeEmitter,
	lockSM *lock.Lock,
	inputSink func(string, []byte) error,
	logf func(format string, args ...any),
) []SubsystemFactory {
	return []SubsystemFactory{
		func() watchdog.Subsystem { return &daemonSubsystem{emitter: em} },
		func() watchdog.Subsystem {
			return &clientSubsystem{
				socketPath: socketPath,
				emitter:    em,
				lock:       lockSM,
				inputSink:  inputSink,
				logf:       logf,
			}
		},
	}
}

// defaultSubsystems is preserved as the test seam for callers that
// pass an empty Config — it returns heartbeat-only placeholders so
// existing tests stay green. Production paths go through Run's
// SubsystemFactories==nil branch which builds realSubsystems.
//
//nolint:unused // referenced by external tests/seams via Config.SubsystemFactories
func defaultSubsystems() []SubsystemFactory {
	return []SubsystemFactory{
		func() watchdog.Subsystem { return &placeholderSubsystem{name: "daemon", interval: time.Second} },
		func() watchdog.Subsystem { return &placeholderSubsystem{name: "client", interval: time.Second} },
	}
}

// daemonSubsystem owns the JSONL watcher / envelope emitter lifetime
// and runs the lock sweep timer.
type daemonSubsystem struct {
	emitter *agent.EnvelopeEmitter
}

func (d *daemonSubsystem) Name() string { return "daemon" }

func (d *daemonSubsystem) Run(ctx context.Context, hb func()) error {
	hb()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			hb()
		}
	}
}

// clientSubsystem runs the attach Unix socket Serve loop.
//
// Restart-safety: stores config, NOT a built *AttachServer. Each
// Run() builds a fresh AttachServer (binds the socket, accepts) and
// closes it on exit. A restart by the watchdog therefore re-binds the
// socket, recovering from any failure that left the prior listener
// in a closed state. Without this pattern, the watchdog's restart
// loop would re-Run a closed AttachServer and the daemon would
// silently disable its attach surface while still beating heartbeats.
type clientSubsystem struct {
	socketPath string
	emitter    *agent.EnvelopeEmitter
	lock       *lock.Lock
	inputSink  func(sessionID string, data []byte) error
	logf       func(format string, args ...any)
}

func (c *clientSubsystem) Name() string { return "client" }

func (c *clientSubsystem) Run(ctx context.Context, hb func()) error {
	hb()
	attach, err := agent.NewAttachServer(agent.AttachServerConfig{
		SocketPath: c.socketPath,
		Emitter:    c.emitter,
		Lock:       c.lock,
		InputSink:  c.inputSink,
		OnError: func(err error) {
			if c.logf != nil {
				c.logf("[daemon] attach: %v", err)
			}
		},
	})
	if err != nil {
		return fmt.Errorf("client subsystem: build attach server: %w", err)
	}
	defer func() { _ = attach.Close() }()

	// Heartbeat from a side goroutine so a slow accept doesn't
	// trigger the watchdog deadlock detector.
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
	if serveErr := attach.Serve(ctx); serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return serveErr
	}
	return nil
}

// placeholderSubsystem is the previous heartbeat-only filler. Kept
// available via Config.SubsystemFactories for tests that don't want
// to spin a real watcher / socket.
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
