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
	"github.com/piaobeizu/tether/internal/cc"
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

	// EnableAuthBroker, when true, constructs an AuthBroker bound to
	// the daemon's envelope emitter and wires it into the attach
	// socket so auth.tool-decision input frames route through the
	// broker. The broker is the daemon-side surface that the cc
	// HookServer's PreToolUse handler routes into via
	// agent.BrokerHookHandlers. When EnableAuthBroker=true, the
	// hookSubsystem is also enabled so cc spawns can hit the loopback
	// hook endpoints and the broker actually fires.
	EnableAuthBroker bool

	// HookSettingsDir is the directory the daemon writes its cc
	// settings.json into (one file per daemon instance, overwritten on
	// each Run). The path of the resulting `<dir>/settings.json` is
	// surfaced via OnHookSettingsReady so cc spawn paths can pass
	// `--settings <path>` (see internal/backend/claude.SpawnOpts).
	//
	// Empty resolves to `$HOME/.tether/cc-settings/`. The directory is
	// created (0700) at Run() time. Production callers normally leave
	// this empty; tests inject a temp dir.
	//
	// Only consulted when EnableAuthBroker=true (no broker → no
	// hookserver → no settings.json to write).
	HookSettingsDir string

	// OnHookSettingsReady, when non-nil, is invoked once after the
	// hookserver listens and `<HookSettingsDir>/settings.json` has been
	// written. The argument is the absolute path to settings.json,
	// suitable for forwarding to cc as `--settings`. Empty / not
	// invoked when EnableAuthBroker=false.
	//
	// The integrating layer (typically cmd/tether's daemon command, or
	// a test harness) plumbs this path into the cc spawn site. The
	// daemon itself never spawns cc — that's the session-owner's job
	// — so this callback is the only seam.
	OnHookSettingsReady func(settingsPath string)

	// SubsystemFactories lets tests install fake subsystems in place
	// of the real daemon/client. nil → use defaultSubsystems with
	// the components built from this Config.
	SubsystemFactories []SubsystemFactory

	// LockAuditLogPath is the path the lock state-machine's JSONL
	// audit log is written to (spec §11.D "audit log" row). Empty
	// resolves to
	// `$HOME/.tether/users/default/sessions/<sid>/lock.log`, where
	// `<sid>` is derived from LockSessionID (or "default" if also
	// empty — v0.1 has no real per-session lock yet).
	//
	// Tests override to keep filesystem state hermetic.
	LockAuditLogPath string

	// LockSessionID names the session bucket the lock audit log
	// lives under. v0.1 has a single process-wide lock (the daemon
	// hasn't been split per-session yet — see follow-up §11.D), so
	// this is effectively a stable label. Empty → "default".
	LockSessionID string

	// LockUser names the user bucket. Empty → "default" — v0.1 is
	// single-user (xiaokang.w@gmicloud.ai per env), and the spec
	// path under `~/.tether/users/default/...` is the documented
	// placeholder. Multi-user is a follow-up tracked in §11.D.
	LockUser string

	// DisableLockAudit disables persistent audit-log writing even
	// when LockAuditLogPath could be resolved. Useful for `tether
	// daemon --no-audit` scenarios + tests that don't want the
	// fixture filesystem touched. Default false (audit log is on
	// for production).
	DisableLockAudit bool

	// EnableStubSweeper opts in to the GC stub-session sweeper
	// (internal/daemon/sweeper.go). Default false for v0.1: ship
	// behind a flag, validate on real workloads, then flip default.
	// When true, the sweeper subsystem joins daemon + client under
	// the watchdog and periodically deletes idle, low-line-count
	// JSONL files in ProjectsDir whose sessions have no live
	// subscribers. See sweeper.go for threshold rationale.
	EnableStubSweeper bool
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
	// logf is the verbose-gated logger — used for routine progress
	// (heartbeats, frame counts, etc.) where the operator can opt in
	// via `tether daemon -v`. In production default (-v off), these
	// are silently dropped.
	logf := func(format string, args ...any) {
		if cfg.Verbose {
			fmt.Fprintf(stderr, format+"\n", args...)
		}
	}
	// warnf is the always-on logger for events the user CANNOT
	// diagnose without seeing them: audit-log file open failures,
	// hookserver serve errors, broker emitter Inject errors. Goes to
	// stderr regardless of the `-v` flag — these are rare enough they
	// don't constitute log spam, and silencing them in default prod
	// (which is what -v=false used to do) makes intermittent failures
	// invisible. cmd/tether sets stderr=os.Stderr unconditionally so
	// production sees these.
	warnf := func(format string, args ...any) {
		fmt.Fprintf(stderr, format+"\n", args...)
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

		// Resolve audit-log path per §11.D and wire the JSONL sink.
		// On any resolution / open error we degrade to in-memory-
		// only History and surface a verbose log line — the daemon
		// MUST stay up even if the audit-log path is unwritable.
		lockOpts := []lock.Option{}
		var lockSink *lock.JSONLLogSink
		if !cfg.DisableLockAudit {
			auditPath, perr := resolveLockAuditPath(cfg)
			if perr != nil {
				warnf("[daemon] lock audit log: resolve path: %v (continuing in-memory only)", perr)
			} else {
				sink, serr := lock.NewJSONLLogSink(auditPath)
				if serr != nil {
					warnf("[daemon] lock audit log: open %q: %v (continuing in-memory only)", auditPath, serr)
				} else {
					lockSink = sink
					lockOpts = append(lockOpts,
						lock.WithLogSink(sink),
						lock.WithSinkErrorHandler(func(err error) {
							warnf("[daemon] lock audit log: append: %v", err)
						}),
					)
					logf("[daemon] lock audit log → %s", auditPath)
				}
			}
		}
		lockSM := lock.New(lockOpts...)

		var broker *agent.AuthBroker
		if cfg.EnableAuthBroker {
			broker, err = agent.NewAuthBroker(emitter)
			if err != nil {
				_ = emitter.Close()
				return fmt.Errorf("daemon: auth broker: %w", err)
			}
		}

		attach, err := agent.NewAttachServer(agent.AttachServerConfig{
			SocketPath: socketPath,
			Emitter:    emitter,
			Lock:       lockSM,
			InputSink:  cfg.InputSink,
			AuthBroker: broker,
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

		// Hook server — only stood up when we have a broker to drive
		// PreToolUse decisions. Without a broker the hookserver would
		// fail-closed on every tool call (noopHandlers default), which
		// is worse than just not pointing cc at it. The settings.json
		// file is written eagerly here (before the supervisor starts)
		// so OnHookSettingsReady fires before any cc spawn could ask
		// for it — single-shot, watchdog restarts re-bind the listener
		// but reuse the prior settings file path (the URL inside is
		// stale until next Run, see hookSubsystem.Run for the
		// per-restart rewrite).
		var hookSrv *cc.HookServer
		var hookSettingsPath string
		if cfg.EnableAuthBroker {
			handlers := agent.NewBrokerHookHandlers(broker)
			hookSrv = cc.NewHookServer(handlers)
			if err := hookSrv.Start(ctx); err != nil {
				_ = emitter.Close()
				return fmt.Errorf("daemon: hook server start: %w", err)
			}
			settingsDir := cfg.HookSettingsDir
			if settingsDir == "" {
				home, herr := os.UserHomeDir()
				if herr != nil {
					_ = hookSrv.Stop(context.Background())
					_ = emitter.Close()
					return fmt.Errorf("daemon: resolve $HOME for hook settings dir: %w", herr)
				}
				settingsDir = filepath.Join(home, ".tether", "cc-settings")
			}
			if err := os.MkdirAll(settingsDir, 0o700); err != nil {
				_ = hookSrv.Stop(context.Background())
				_ = emitter.Close()
				return fmt.Errorf("daemon: mkdir hook settings dir %q: %w", settingsDir, err)
			}
			baseURL := "http://" + hookSrv.Addr()
			hookSettingsPath, err = cc.WriteSettingsFile(settingsDir, baseURL)
			if err != nil {
				_ = hookSrv.Stop(context.Background())
				_ = emitter.Close()
				return fmt.Errorf("daemon: write cc settings: %w", err)
			}
			logf("[daemon] hook server listening at %s; cc settings → %s", baseURL, hookSettingsPath)
			if cfg.OnHookSettingsReady != nil {
				cfg.OnHookSettingsReady(hookSettingsPath)
			}
		}

		factories = realSubsystems(socketPath, emitter, lockSM, cfg.InputSink, broker, logf)
		if hookSrv != nil {
			factories = append(factories, hookSubsystemFactory(hookSrv, logf))
		}
		if cfg.EnableStubSweeper {
			factories = append(factories, gcSubsystemFactory(stubSweeperConfig{
				ProjectsDir: projectsDir,
				Emitter:     emitter,
				Logf:        logf,
			}))
		}
		// emitter lives for the daemon's full lifetime; clean up
		// on Run exit. clientSubsystem owns its per-run AttachServer
		// and closes it inside Run's defer. lockSink (if any) flushes
		// + releases its file handle here. hookSrv (if any) is
		// gracefully shut down — its watchdog subsystem also stops the
		// server when ctx cancels, but a defensive Stop here covers
		// the path where Run returns before factories run.
		defer func() {
			if broker != nil {
				broker.Close()
			}
			if hookSrv != nil {
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_ = hookSrv.Stop(shutdownCtx)
				cancel()
			}
			_ = emitter.Close()
			if lockSink != nil {
				_ = lockSink.Close()
			}
		}()
	}

	for _, f := range factories {
		wd.Supervise(f())
	}
	return wd.Run(ctx)
}

// resolveLockAuditPath maps daemon Config to the spec §11.D audit log
// path:
//
//	~/.tether/users/<user>/sessions/<sid>/lock.log
//
// Empty `LockAuditLogPath` → resolves $HOME and uses the default
// layout. Empty user/sid default to "default" (v0.1 is single-user,
// single-session-bucket; multi-tenant is a §11.D follow-up).
func resolveLockAuditPath(cfg Config) (string, error) {
	if cfg.LockAuditLogPath != "" {
		return cfg.LockAuditLogPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve $HOME: %w", err)
	}
	user := cfg.LockUser
	if user == "" {
		user = "default"
	}
	sid := cfg.LockSessionID
	if sid == "" {
		sid = "default"
	}
	return filepath.Join(home, ".tether", "users", user, "sessions", sid, "lock.log"), nil
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
	broker *agent.AuthBroker,
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
				broker:     broker,
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
	broker     *agent.AuthBroker // nil unless EnableAuthBroker=true
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
		AuthBroker: c.broker,
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

// hookSubsystem owns the lifetime of the cc.HookServer the daemon
// spun up before factories ran. The HookServer's listener was bound
// in Run() (so the address could be baked into settings.json before
// any cc spawn could need it); this subsystem keeps the goroutine
// supervised — heartbeats while alive, cleanly stops on ctx cancel.
//
// Restart-unsafety note: HookServer is single-use (Stop ⇒
// ErrServerStopped on the next Start). If Run() returns we tear it
// down for good. The watchdog therefore should NOT auto-restart this
// subsystem in v0.1 — but the watchdog has no per-subsystem opt-out
// today, so a restart would surface ErrServerStopped here and the
// supervisor would retry forever. A deliberate hardening follow-up
// is to make HookServer reusable; until then, hookSubsystem.Run
// returns a non-restartable error path on second-Start.
type hookSubsystem struct {
	srv  *cc.HookServer
	logf func(format string, args ...any)
}

func (h *hookSubsystem) Name() string { return "hook" }

func (h *hookSubsystem) Run(ctx context.Context, hb func()) error {
	hb()
	// Heartbeat side goroutine so the supervisor's deadlock detector
	// stays happy while we just sit waiting for ctx.
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			// Cleanly shut down. Stop blocks until the Serve goroutine
			// exits, so when this returns the listener fd is released.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.srv.Stop(shutdownCtx); err != nil && h.logf != nil {
				h.logf("[daemon] hook server stop: %v", err)
			}
			// Surface the latest serve error if any (fd exhaustion etc.)
			// so the watchdog logs reflect the cause; but treat
			// ctx.Done as a clean exit overall.
			if se := h.srv.ServeErr(); se != nil && h.logf != nil {
				h.logf("[daemon] hook server serve error: %v", se)
			}
			return nil
		case <-t.C:
			hb()
			// Surface a serve-time error promptly so the watchdog
			// observes it (keeps "subsystem alive" honest).
			if se := h.srv.ServeErr(); se != nil {
				return fmt.Errorf("hook server: %w", se)
			}
		}
	}
}

// hookSubsystemFactory returns a SubsystemFactory that wraps a
// pre-bound HookServer. The factory is single-shot — calling it twice
// would re-Run the same (now-stopped) server, which fails. The
// watchdog calls each factory exactly once, so this is fine for v0.1.
func hookSubsystemFactory(srv *cc.HookServer, logf func(format string, args ...any)) SubsystemFactory {
	return func() watchdog.Subsystem {
		return &hookSubsystem{srv: srv, logf: logf}
	}
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
