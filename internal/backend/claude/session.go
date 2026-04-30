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
// forever. Step 6 (Recover) reuses InitTimeout when re-establishing.
const (
	InitTimeout  = 10 * time.Second
	CloseTimeout = 10 * time.Second
)

// ErrInitTimeout is returned by New when the subprocess does not emit
// system/init within InitTimeout. Spec §6.A.4.
var ErrInitTimeout = errors.New("claude: timed out waiting for system/init")

// ErrSubprocessGone is returned by Send when stdin write fails because the
// subprocess has exited (EPIPE / closed pipe). Caller should differentiate
// this from generic write errors and trigger Session.Recover. Spec §6.A.3.
var ErrSubprocessGone = errors.New("claude: subprocess gone (stdin closed)")

// ErrSessionClosed is returned when operations are attempted on a Session
// that has already been Close()'d.
var ErrSessionClosed = errors.New("claude: session closed")

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

	sub      *Subprocess
	control  *Controller
	parserCh <-chan Envelope

	// Mutable state — guarded by mu.
	mu        sync.RWMutex
	sessionID string
	state     SessionState
	closed    bool

	events chan Envelope // forwarded to consumer (control_* not included)
	initCh chan string   // first system/init session_id lands here

	stderrMu  sync.Mutex
	stderrBuf bytes.Buffer

	// Background goroutine signals.
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
		ProjectCwd:   opts.ProjectCwd,
		sub:          sub,
		control:      NewController(sub.Stdin, auth),
		events:       make(chan Envelope, 64),
		initCh:       make(chan string, 1),
		dispatchDone: make(chan struct{}),
		stderrDone:   make(chan struct{}),
		ctx:          subCtx,
		cancel:       cancel,
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
func (s *Session) startWithInitTimeout(ctx context.Context, initialMessage string, initTimeout time.Duration) error {
	if s.SessionID() != "" {
		// Already initialized — Start is being called as a "send first
		// message" convenience. Just delegate to Send.
		return s.Send(initialMessage)
	}

	if err := s.Send(initialMessage); err != nil {
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
	s.mu.RLock()
	sid := s.sessionID
	closed := s.closed
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

	if _, err := s.sub.Stdin.Write(b); err != nil {
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
	_, err := s.control.SendInterrupt(ctx)
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
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		s.closeErr = s.terminateAndWait()
		s.cancel() // tear down spawn ctx + parser

		// Wait for background goroutines (with a sanity bound).
		select {
		case <-s.dispatchDone:
		case <-time.After(2 * time.Second):
		}
		select {
		case <-s.stderrDone:
		case <-time.After(2 * time.Second):
		}
	})
	return s.closeErr
}

func (s *Session) terminateAndWait() error {
	proc := s.sub.Cmd.Process
	if proc == nil {
		return nil
	}

	// Best-effort SIGTERM first.
	_ = proc.Signal(syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		_ = s.sub.WaitOnce()
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
func (s *Session) dispatch() {
	defer close(s.dispatchDone)
	defer close(s.events)

	sentInit := false
	for env := range s.parserCh {
		// Controller eats control_request / control_response.
		if s.control.Dispatch(s.ctx, env) {
			continue
		}

		s.applyStateTransition(env)

		if env.Type == EventSystem && env.Subtype == "init" && env.SessionID != "" {
			s.mu.Lock()
			s.sessionID = env.SessionID
			s.mu.Unlock()
			if !sentInit {
				sentInit = true
				select {
				case s.initCh <- env.SessionID:
				default:
				}
			}
		}

		select {
		case s.events <- env:
		case <-s.ctx.Done():
			return
		}
	}
}

// applyStateTransition updates the SessionState based on incoming events.
// Coarse for v0.1 — Step 6 elaborates with mid-text / tool-pending
// distinctions for the RecoverGate.
func (s *Session) applyStateTransition(env Envelope) {
	switch env.Type {
	case EventResult:
		s.mu.Lock()
		s.state = StateIdle
		s.mu.Unlock()
	case EventStreamEvent:
		s.mu.RLock()
		st := s.state
		s.mu.RUnlock()
		// Only transition to Streaming if currently Idle — preserve
		// ToolPending / Streaming once set.
		if st == StateIdle {
			s.mu.Lock()
			s.state = StateStreaming
			s.mu.Unlock()
		}
	}
}

// drainStderr continuously reads stderr into a capped buffer so the
// subprocess pipe never blocks on a full kernel buffer (spec §6.A.5).
func (s *Session) drainStderr() {
	defer close(s.stderrDone)
	chunk := make([]byte, 4096)
	for {
		n, err := s.sub.Stderr.Read(chunk)
		if n > 0 {
			s.appendStderr(chunk[:n])
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) appendStderr(p []byte) {
	s.stderrMu.Lock()
	defer s.stderrMu.Unlock()
	if s.stderrBuf.Len()+len(p) > stderrCap {
		// Trim from the front to keep last stderrCap bytes.
		excess := s.stderrBuf.Len() + len(p) - stderrCap
		if excess >= s.stderrBuf.Len() {
			s.stderrBuf.Reset()
		} else {
			rest := s.stderrBuf.Bytes()[excess:]
			s.stderrBuf.Reset()
			s.stderrBuf.Write(rest)
		}
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
