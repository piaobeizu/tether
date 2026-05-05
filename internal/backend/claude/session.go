package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// Default timeouts. Per spec §6.A.4 the system/init wait MUST have a hard
// upper bound — without it a bogus --resume can leave the daemon hanging
// forever. IdleWaitTimeout is the watchdog used by Recover (§6.C.x) to
// decide a stuck non-idle state can be forced past.
const (
	InitTimeout     = 10 * time.Second
	CloseTimeout    = 10 * time.Second
	IdleWaitTimeout = 120 * time.Second
)

// ErrInitTimeout is returned by Start when the subprocess does not emit
// system/init within InitTimeout. Spec §6.A.4.
var ErrInitTimeout = errors.New("claude: timed out waiting for system/init")

// ErrSubprocessGone is returned by Send when stdin write fails because the
// subprocess has exited (EPIPE / closed pipe). Caller should differentiate
// this from generic write errors and trigger Session.Recover. Spec §6.A.3.
var ErrSubprocessGone = errors.New("claude: subprocess gone (stdin closed)")

// ErrSessionClosed is returned when operations are attempted on a Session
// that has already been Close()'d.
var ErrSessionClosed = errors.New("claude: session closed")

// ErrUnsafeToReload is returned by Recover when the session is in a
// non-idle state and has not been stuck long enough to qualify for the
// stale-state override. Spec §6.C.1–C.3.
var ErrUnsafeToReload = errors.New("claude: recover refused — session not idle (C.1–C.3)")

// ErrSessionNotStarted is returned by Recover when called before any
// system/init has populated SessionID — there's nothing to --resume.
var ErrSessionNotStarted = errors.New("claude: recover requires a started session (no session_id captured)")

// SessionState models where the conversation is in the turn lifecycle.
// Used by Step 6's RecoverGate to decide whether Recover() is safe.
type SessionState int

const (
	StateIdle        SessionState = iota // initial; also after `result` event
	StateStreaming                       // received user msg, awaiting result
	StateToolPending                     // assistant emitted tool_use, awaiting tool_result
)

func (s SessionState) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateStreaming:
		return "streaming"
	case StateToolPending:
		return "tool-pending"
	}
	return "unknown"
}

// stderrCap limits how many trailing bytes of stderr we retain for
// post-mortem inspection. Per spec §6.A.5 stderr MUST be drained
// continuously to avoid blocking the subprocess on a full pipe buffer.
const stderrCap = 64 * 1024

// Session is one running claude subprocess + its parser + control mux.
//
// Concurrency model:
//   - One dispatcher goroutine reads from the parser channel, routes
//     control_* to Controller, tracks state, and forwards everything else
//     to the events channel.
//   - One stderr goroutine continuously copies stderr bytes into a capped
//     ring (last 64KB).
//   - Send() / Close() / state queries are safe from any goroutine.
type Session struct {
	ProjectCwd string

	// Mutable subprocess + per-spawn lifecycle. Replaced wholesale by
	// Recover. Guarded by mu for cross-goroutine reads (control writes go
	// through control which has its own stdinMu).
	sub      *Subprocess
	control  *Controller
	parserCh <-chan Envelope

	// Retained for Recover to re-spawn with the same configuration.
	spawnOpts SpawnOpts
	auth      ToolAuthorizer
	parentCtx context.Context

	// Mutable state — guarded by mu.
	mu             sync.RWMutex
	sessionID      string
	state          SessionState
	stateEnteredAt time.Time
	closed         bool
	sentInit       bool // whether initCh has already been signaled

	// Version drift detection (gh-13 round-1 review Tier-4 / ops-concerns
	// subitem #3). claude_code_version SHOULD be stable across all
	// system/init events of the same session — if it changes mid-session
	// (e.g., user upgraded `claude` binary while a Recover happened), the
	// daemon needs to know so it can refuse-or-warn.
	//
	// recordedClaudeVersion captures the FIRST non-empty version seen and
	// is never overwritten by drift; versionDrifts accumulates each
	// subsequent mismatch. Higher layers (daemon supervisor) decide policy
	// (warn vs refuse) by polling VersionDrifts() after Recover.
	recordedClaudeVersion string
	versionDrifts         []VersionDrift

	// Latest rate_limit_event observed (ops-concerns subitem #4). cc emits
	// this periodically; we cache it so the daemon can pre-flight check
	// "is the user about to hit a limit" before forwarding new user
	// messages. Session itself does NOT enforce — that's a daemon-level
	// policy via EvaluateRateLimit + LastRateLimit().
	lastRateLimit    RateLimitInfo
	hasRateLimit     bool
	lastRateLimitAt  time.Time // wall-clock when last RL was recorded

	// OOM circuit-breaker (ops-concerns subitem #2). Timestamps of recent
	// SIGKILL-attributed exits (i.e., subprocess died on its own with
	// SIGKILL — likely OOM-killer or external `kill -9`). Old timestamps
	// outside the policy window are pruned by recordOOMEventLocked. See
	// oom_circuit.go for policy + ErrOOMCircuitOpen contract.
	oomEvents []time.Time

	events chan Envelope // long-lived; only Close() closes it
	initCh chan string   // first system/init in *current* dispatcher lands here

	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	// Background goroutine signals — replaced on each Recover.
	dispatchDone chan struct{}
	stderrDone   chan struct{}

	closeOnce sync.Once
	closeErr  error

	// External cancellation propagates to spawn ctx + parser.
	ctx    context.Context
	cancel context.CancelFunc
}

// New spawns a claude subprocess and wires the parser + controller +
// stderr drain. It returns immediately (does NOT wait for system/init).
//
// IMPORTANT: claude in --input-format stream-json mode does NOT emit
// system/init until it has read at least one user envelope on stdin.
// Therefore the typical bootstrap pattern is:
//
//	sess, err := New(ctx, opts, auth)
//	... err handling ...
//	if err := sess.Start(ctx, "initial prompt"); err != nil { ... }
//	// SessionID is now populated; drain Events() for the first turn.
//
// This applies to both fresh sessions and --resume sessions.
func New(ctx context.Context, opts SpawnOpts, auth ToolAuthorizer) (*Session, error) {
	subCtx, cancel := context.WithCancel(ctx)

	sub, err := Spawn(subCtx, opts)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &Session{
		ProjectCwd:     opts.ProjectCwd,
		sub:            sub,
		control:        NewController(sub.Stdin, auth),
		spawnOpts:      opts,
		auth:           auth,
		parentCtx:      ctx,
		events:         make(chan Envelope, 64),
		initCh:         make(chan string, 1),
		dispatchDone:   make(chan struct{}),
		stderrDone:     make(chan struct{}),
		ctx:            subCtx,
		cancel:         cancel,
		state:          StateIdle,
		stateEnteredAt: time.Now(),
	}
	s.parserCh = Parse(subCtx, sub.Stdout, ParseOpts{})

	go s.drainStderr()
	go s.dispatch()

	return s, nil
}

// Start sends an initial user message and waits for system/init to arrive
// (with InitTimeout, spec §6.A.4). On timeout the session is Close()'d
// before ErrInitTimeout is returned, so callers don't need to clean up.
//
// Start is idempotent only in the sense of "calling it twice is safe" —
// the second call sends another message but returns immediately since
// SessionID is already populated. To send subsequent messages use Send.
func (s *Session) Start(ctx context.Context, initialMessage string) error {
	return s.startWithInitTimeout(ctx, initialMessage, InitTimeout)
}

// startWithInitTimeout is the test seam for A.4 timeout verification.
//
// L1: every error path Close()s the Session — otherwise the caller leaks
// subprocess + 3 pipes + 2 goroutines. The most common failure
// (ErrSubprocessGone on first Send) was previously the leakiest path.
func (s *Session) startWithInitTimeout(ctx context.Context, initialMessage string, initTimeout time.Duration) error {
	if s.SessionID() != "" {
		// Already initialized — Start is being called as a "send first
		// message" convenience. Just delegate to Send.
		return s.Send(initialMessage)
	}

	if err := s.Send(initialMessage); err != nil {
		_ = s.Close()
		return err
	}

	timer := time.NewTimer(initTimeout)
	defer timer.Stop()

	select {
	case sid := <-s.initCh:
		s.mu.Lock()
		s.sessionID = sid
		s.mu.Unlock()
		return nil
	case <-timer.C:
		_ = s.Close()
		return ErrInitTimeout
	case <-ctx.Done():
		_ = s.Close()
		return ctx.Err()
	}
}

// SessionID returns the canonical session_id captured from the most recent
// system/init event. Per spec §7.7 resume mode may emit init twice; we keep
// the latest value.
func (s *Session) SessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionID
}

// VersionDrift records one mismatch between claude_code_version values seen
// in successive system/init events. See Session.versionDrifts.
type VersionDrift struct {
	From string    // recorded version before the drift event
	To   string    // version observed in the new system/init
	At   time.Time // wall-clock when the drift was recorded
}

// ClaudeCodeVersion returns the claude_code_version recorded from the FIRST
// non-empty system/init seen on this Session. Subsequent drifts do NOT
// overwrite this — see VersionDrifts() for those.
func (s *Session) ClaudeCodeVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recordedClaudeVersion
}

// VersionDrifts returns a copy of all observed claude_code_version mismatches
// for this Session. Empty slice = no drift; >0 = the binary has changed
// underneath us at least once. Higher layers (daemon supervisor) decide what
// to do (warn-only / refuse Recover / surface to user).
func (s *Session) VersionDrifts() []VersionDrift {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.versionDrifts) == 0 {
		return nil
	}
	out := make([]VersionDrift, len(s.versionDrifts))
	copy(out, s.versionDrifts)
	return out
}

// LastRateLimit returns the latest rate_limit_info recorded from a
// rate_limit_event, plus a bool indicating whether any has been seen. When
// ok is false, the returned RateLimitInfo is zero-valued.
//
// Used by daemon-level pre-flight checks (see EvaluateRateLimit). Session
// itself does NOT enforce on Send — keeping the layering: Session records,
// daemon supervisor decides policy.
func (s *Session) LastRateLimit() (info RateLimitInfo, recordedAt time.Time, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastRateLimit, s.lastRateLimitAt, s.hasRateLimit
}

// recordRateLimit captures a rate_limit_event into Session state. Centralized
// so dispatch can call it and unit tests can drive it without a subprocess.
// Bad envelopes (decode errors) are silently ignored — rate-limit observation
// is best-effort.
func (s *Session) recordRateLimit(env Envelope) {
	r, err := env.DecodeRateLimit()
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRateLimit = r.RateLimitInfo
	s.hasRateLimit = true
	s.lastRateLimitAt = time.Now()
}

// recordSystemInit captures session_id + claude_code_version from a
// system/init envelope. Returns true iff initCh has not yet been signaled
// for this Session (caller should forward the session_id to initCh).
//
// Centralized so version drift logic stays in one place and is unit-testable
// without spinning a real subprocess.
func (s *Session) recordSystemInit(env Envelope) bool {
	var newVer string
	if si, err := env.DecodeSystemInit(); err == nil {
		newVer = si.ClaudeCodeVersion
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = env.SessionID
	if s.recordedClaudeVersion == "" {
		// First non-empty observation wins; empty observations don't count.
		s.recordedClaudeVersion = newVer
	} else if newVer != "" && newVer != s.recordedClaudeVersion {
		// Subsequent mismatch — record but DO NOT overwrite recorded.
		s.versionDrifts = append(s.versionDrifts, VersionDrift{
			From: s.recordedClaudeVersion,
			To:   newVer,
			At:   time.Now(),
		})
	}
	if s.sentInit {
		return false
	}
	s.sentInit = true
	return true
}

// State returns the current SessionState. Used by RecoverGate (Step 6).
func (s *Session) State() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// Events returns the channel of non-control envelopes for UI consumption.
// Closed when the subprocess exits or Close() is called.
func (s *Session) Events() <-chan Envelope {
	return s.events
}

// Send writes a user message to the subprocess stdin. Returns
// ErrSubprocessGone on EPIPE / closed-pipe writes (spec §6.A.3),
// ErrSessionClosed if the session was already closed, and other I/O errors
// otherwise.
func (s *Session) Send(text string) error {
	// C2: snapshot mutable session pointers under RLock; do I/O outside
	// the lock so we don't hold mu across a syscall.
	s.mu.RLock()
	sid := s.sessionID
	closed := s.closed
	sub := s.sub
	s.mu.RUnlock()
	if closed {
		return ErrSessionClosed
	}

	msg := NewUserMessage(sid, text)
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal user message: %w", err)
	}
	b = append(b, '\n')

	if _, err := sub.Stdin.Write(b); err != nil {
		if isSubprocessGone(err) {
			return ErrSubprocessGone
		}
		return fmt.Errorf("write stdin: %w", err)
	}

	s.mu.Lock()
	if s.state == StateIdle {
		s.state = StateStreaming
	}
	s.mu.Unlock()
	return nil
}

// SendInterrupt issues an outbound control_request interrupt to cc.
// Convenience wrapper around s.control.SendInterrupt.
func (s *Session) SendInterrupt(ctx context.Context) error {
	// C2: snapshot the controller pointer — Recover may swap it.
	s.mu.RLock()
	ctrl := s.control
	s.mu.RUnlock()
	_, err := ctrl.SendInterrupt(ctx)
	return err
}

// StderrSnapshot returns up to the last 64KB of stderr captured from the
// subprocess. Useful for post-mortem when the process dies unexpectedly.
func (s *Session) StderrSnapshot() []byte {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	out := make([]byte, s.stderrBuf.Len())
	copy(out, s.stderrBuf.Bytes())
	return out
}

// Close terminates the subprocess gracefully (SIGTERM, then SIGKILL after
// CloseTimeout) and waits for all background goroutines to exit. Safe to
// call multiple times — subsequent calls return the cached error from the
// first call.
//
// C6: dispatchDone wait is unbounded. dispatch's inner select includes
// `<-dispatchCtx.Done()` and we cancel above, so dispatch ALWAYS exits
// shortly after — a timeout here would mask a real bug and risk
// closing s.events while dispatch is still sending to it (panic).
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.closeErr = s.terminateAndWait()
		s.cancel() // tear down spawn ctx + parser

		s.mu.RLock()
		dDone := s.dispatchDone
		eDone := s.stderrDone
		s.mu.RUnlock()

		// dispatch must exit (its select includes <-dispatchCtx.Done(),
		// canceled above). Wait unconditionally — see C6 above.
		<-dDone

		// stderr drain may take a moment after pipe close; small bounded
		// wait is fine since stderrBuf is best-effort and not a panic
		// path.
		select {
		case <-eDone:
		case <-time.After(2 * time.Second):
		}

		// Safe to close events — dispatch is done writing to it.
		close(s.events)
	})
	return s.closeErr
}

// Recover terminates the current subprocess and re-spawns claude with
// `--resume <SessionID>`, preserving the event stream continuity.
//
// Spec §6.D.2: this is the unified primitive used by all four
// recovery callers (plugin reload / pod restart / daemon crash / WT
// reconnect). Spec §6.C.1–C.3 require the session to be idle before
// reload; if the state is non-idle, Recover returns ErrUnsafeToReload —
// UNLESS the state has been stuck for ≥ IdleWaitTimeout, in which case
// Recover proceeds (treating the stream as stale / dead).
//
// The events channel persists across Recover; consumers continue
// reading from the same channel. SessionID is preserved (we resume to
// the same conversation). State transitions to Idle.
//
// IMPORTANT: like New, the new subprocess will NOT emit system/init
// until the next Send. Callers should expect to Send a user message
// after Recover to drive the session forward.
func (s *Session) Recover(ctx context.Context) error {
	s.mu.RLock()
	state := s.state
	enteredAt := s.stateEnteredAt
	sid := s.sessionID
	closed := s.closed
	oldSub := s.sub
	oldControl := s.control
	oldCancel := s.cancel
	oldDispatchDone := s.dispatchDone
	oldStderrDone := s.stderrDone
	s.mu.RUnlock()

	if closed {
		return ErrSessionClosed
	}
	if sid == "" {
		return ErrSessionNotStarted
	}
	if state != StateIdle && time.Since(enteredAt) < IdleWaitTimeout {
		return ErrUnsafeToReload
	}

	// OOM circuit-breaker (ops-concerns subitem #2). If too many SIGKILL
	// exits have been attributed to this Session within the policy window,
	// refuse to re-spawn — surface ErrOOMCircuitOpen so the daemon can
	// degrade gracefully instead of looping forever.
	s.mu.RLock()
	tripped := s.oomCircuitTrippedLocked(time.Now())
	s.mu.RUnlock()
	if tripped {
		return ErrOOMCircuitOpen
	}

	// C5: kill subprocess BEFORE AbortPending. Order matters — if we
	// abort first, a racing outbound() can register in the freshly-empty
	// pending map, write to the still-live stdin, then block forever
	// waiting for a response from a soon-to-be-dead process.
	//
	// ops-concerns subitem #2: liveness probe via signal(0) BEFORE the
	// kill so we can later distinguish "OOM-killer killed it" from "we
	// killed it during teardown" — only the former counts toward the
	// circuit-breaker. signal(0) returns nil if proc is alive, error if
	// already gone.
	preKilledByUs := false
	if proc := oldSub.Cmd.Process; proc != nil {
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			_ = proc.Kill()
			preKilledByUs = true
		}
	}
	oldControl.AbortPending(ErrRecoverInterrupted)
	oldCancel()

	// C3: dispatch always exits via its <-dispatchCtx.Done() arm
	// (canceled above) — wait unbounded. A timeout here masks bugs and
	// risks the new dispatch starting alongside an old one stuck in
	// `s.events <- env` (concurrent writes / data interleave).
	<-oldDispatchDone
	select {
	case <-oldStderrDone:
	case <-time.After(5 * time.Second):
	}
	waitErr := oldSub.WaitOnce()

	// ops-concerns subitem #2: if we did NOT kill the subprocess (i.e.
	// it died on its own before our liveness probe) and its exit was a
	// SIGKILL, attribute to OOM-killer (or external kill -9) and record
	// the event. The next Recover invocation will check the circuit at
	// the top and refuse if we've crossed the threshold.
	if !preKilledByUs && isOOMExit(waitErr) {
		s.mu.Lock()
		s.recordOOMEventLocked(time.Now())
		s.mu.Unlock()
	}

	// C4: re-check closed after teardown — Close() may have run
	// concurrently. Without this we'd Spawn a new subprocess that
	// nobody owns, then Close() would later close(s.events) on a live
	// dispatcher (panic).
	s.mu.RLock()
	closedNow := s.closed
	s.mu.RUnlock()
	if closedNow {
		return ErrSessionClosed
	}

	// Spawn a fresh subprocess against the same session_id.
	subCtx, cancel := context.WithCancel(s.parentCtx)
	opts := s.spawnOpts
	opts.SessionID = sid

	sub, err := Spawn(subCtx, opts)
	if err != nil {
		cancel()
		// L2: spawn-failure during Recover leaves the Session in an
		// incoherent half-replaced state. Mark it closed so callers
		// can observe ErrSessionClosed on subsequent operations.
		s.markUnrecoverable()
		return fmt.Errorf("recover: spawn (session unrecoverable): %w", err)
	}

	// Replace internals atomically.
	s.mu.Lock()
	s.sub = sub
	s.control = NewController(sub.Stdin, s.auth)
	s.parserCh = Parse(subCtx, sub.Stdout, ParseOpts{})
	s.dispatchDone = make(chan struct{})
	s.stderrDone = make(chan struct{})
	s.initCh = make(chan string, 1)
	s.ctx = subCtx
	s.cancel = cancel
	s.state = StateIdle
	s.stateEnteredAt = time.Now()
	s.sentInit = false // next system/init will signal again
	s.stderrBuf.Reset()
	s.mu.Unlock()

	// Restart goroutines.
	go s.drainStderr()
	go s.dispatch()

	return nil
}

// markUnrecoverable marks the session closed and closes the events
// channel exactly once. Used by Recover when re-Spawn fails after the
// old subprocess has already been torn down — the Session is no longer
// in a coherent state, so we surface ErrSessionClosed on subsequent
// operations rather than letting callers act on a half-replaced struct.
func (s *Session) markUnrecoverable() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.events)
	})
}

func (s *Session) terminateAndWait() error {
	// C2: snapshot s.sub under RLock — Recover may swap it concurrently
	// (though Close + Recover both should be racing-incompatible, the
	// race detector still flags the unprotected read).
	s.mu.RLock()
	sub := s.sub
	s.mu.RUnlock()

	proc := sub.Cmd.Process
	if proc == nil {
		return nil
	}

	// Best-effort SIGTERM first.
	_ = proc.Signal(syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		_ = sub.WaitOnce()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(CloseTimeout):
		_ = proc.Kill()
		<-done
		return nil
	}
}

// dispatch routes parser events: control_* to Controller, system/init
// captures sessionID, result/stream_event drive the state machine, and
// everything else (assistant/user/result/system/* etc.) is forwarded on
// the events channel for UI consumption.
//
// dispatch does NOT close the events channel — that's Close()'s job.
// Recover() restarts dispatch on a fresh parser channel; events stay
// continuous across restarts so consumers don't see a re-key.
func (s *Session) dispatch() {
	// Snapshot parser channel + ctx — Recover replaces these wholesale.
	s.mu.RLock()
	parserCh := s.parserCh
	dispatchCtx := s.ctx
	control := s.control
	dispatchDone := s.dispatchDone
	s.mu.RUnlock()
	defer close(dispatchDone)

	for env := range parserCh {
		// Controller eats control_request / control_response.
		if control.Dispatch(dispatchCtx, env) {
			continue
		}

		s.applyStateTransition(env)

		if env.Type == EventSystem && env.Subtype == "init" && env.SessionID != "" {
			if shouldSignal := s.recordSystemInit(env); shouldSignal {
				select {
				case s.initCh <- env.SessionID:
				default:
				}
			}
		}

		if env.Type == EventRateLimit {
			s.recordRateLimit(env)
		}

		select {
		case s.events <- env:
		case <-dispatchCtx.Done():
			return
		}
	}
}

// applyStateTransition updates the SessionState based on incoming events.
//
// State machine (spec §6.C.1–C.3):
//
//	Idle           after `result` event (default initial state)
//	Streaming      assistant is producing tokens
//	ToolPending    assistant emitted tool_use, awaiting tool_result echo
//
// Transitions tracked here gate Recover() in Step 6 — see safetyToReload.
func (s *Session) applyStateTransition(env Envelope) {
	switch env.Type {
	case EventResult:
		s.setState(StateIdle)
	case EventStreamEvent:
		// Look at the inner stream_event for tool_use stop reason.
		if isToolUseStop(env) {
			s.setState(StateToolPending)
			return
		}
		// Generic streaming token / message_stop / etc.
		s.mu.RLock()
		st := s.state
		s.mu.RUnlock()
		if st == StateIdle {
			s.setState(StateStreaming)
		}
	case EventUser:
		// A `user` event carrying tool_result transitions back to streaming.
		if hasToolResult(env) {
			s.setState(StateStreaming)
		}
	}
}

// setState updates state + records the transition timestamp for the
// stale-state watchdog in Recover.
func (s *Session) setState(next SessionState) {
	s.mu.Lock()
	if s.state != next {
		s.state = next
		s.stateEnteredAt = time.Now()
	}
	s.mu.Unlock()
}

// isToolUseStop returns true when env is a stream_event whose nested
// `delta.stop_reason == "tool_use"`. We re-decode the raw bytes since the
// stream_event payload is not in our typed union.
func isToolUseStop(env Envelope) bool {
	if env.Type != EventStreamEvent {
		return false
	}
	var probe struct {
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal(env.Raw, &probe); err != nil {
		return false
	}
	return probe.Event.Type == "message_delta" && probe.Event.Delta.StopReason == "tool_use"
}

// hasToolResult returns true when env is a `user` envelope containing a
// tool_result content block.
func hasToolResult(env Envelope) bool {
	if env.Type != EventUser {
		return false
	}
	var probe struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(env.Raw, &probe); err != nil {
		return false
	}
	for _, c := range probe.Message.Content {
		if c.Type == "tool_result" {
			return true
		}
	}
	return false
}

// drainStderr continuously reads stderr into a capped buffer so the
// subprocess pipe never blocks on a full kernel buffer (spec §6.A.5).
//
// C1: snapshot s.sub.Stderr and s.stderrDone at start of the goroutine.
// Recover replaces both fields wholesale; without snapshotting, the
// `defer close(s.stderrDone)` would fire on the NEW channel (created by
// Recover) — a double-close panic when the new drainStderr also closes
// it — and Read() would race between the old and new pipe.
func (s *Session) drainStderr() {
	s.mu.RLock()
	stderr := s.sub.Stderr
	done := s.stderrDone
	s.mu.RUnlock()
	defer close(done)

	chunk := make([]byte, 4096)
	for {
		n, err := stderr.Read(chunk)
		if n > 0 {
			s.appendStderr(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}

// appendStderr appends p to the bounded stderr ring buffer per spec §A.5.
//
// Invariant: after this call, s.stderrBuf.Len() <= stderrCap. We always keep
// the last stderrCap bytes; older content is dropped (front trim).
//
// ops-concerns subitem #1 fix: previously, when len(p) alone exceeded
// stderrCap the existing buffer was Reset() but the entire oversized p was
// then appended, leaving the buffer >stderrCap (contract violation). With
// 4KB read chunks in drainStderr this never bit in practice, but it would
// trigger the moment a single Read returned more bytes than the cap.
func (s *Session) appendStderr(p []byte) {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()

	// Case A: single write itself meets/exceeds cap → drop everything we had,
	// keep only the last stderrCap bytes of p.
	if len(p) >= stderrCap {
		s.stderrBuf.Reset()
		s.stderrBuf.Write(p[len(p)-stderrCap:])
		return
	}

	// Case B: combined would exceed cap → trim front of existing buffer
	// to make room while keeping len ≤ cap after the upcoming Write(p).
	if s.stderrBuf.Len()+len(p) > stderrCap {
		excess := s.stderrBuf.Len() + len(p) - stderrCap
		rest := s.stderrBuf.Bytes()[excess:]
		s.stderrBuf.Reset()
		s.stderrBuf.Write(rest)
	}
	s.stderrBuf.Write(p)
}

// isSubprocessGone returns true when err indicates the subprocess has
// closed its stdin (most commonly EPIPE on write).
func isSubprocessGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, os.ErrClosed) {
		return true
	}
	return false
}
