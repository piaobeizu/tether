package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenCodeProvider implements AgentProvider for opencode (D-17a A2).
// It starts a long-lived `opencode serve` child process and subscribes to its
// SSE /global/event stream for token-level streaming (message.part.delta).
// Prompts are delivered via `opencode run --attach <url> --format json`.
type OpenCodeProvider struct{}

func NewOpenCodeProvider() *OpenCodeProvider { return &OpenCodeProvider{} }

func (p *OpenCodeProvider) Name() string { return "opencode" }

func (p *OpenCodeProvider) Spawn(ctx context.Context, cfg SpawnConfig) (Session, error) {
	workdir := cfg.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}

	sess := &opencodeSession{
		workdir:   workdir,
		env:       cfg.Env,
		spawnCtx:  ctx,
		events:    make(chan Event, 64),
		sidCh:     make(chan struct{}),
		resumeSID: cfg.ResumeSessionID,
	}

	if err := sess.startServe(ctx); err != nil {
		return nil, err
	}

	// When the session context ends, release any SessionID() waiters and close
	// the event stream. Each per-serve sseLoop deliberately does NOT own these
	// (a hibernated/relaunched serve must not tear down the session's event
	// stream), so this single session-scoped goroutine covers the ctx-cancel
	// teardown paths that don't go through Close().
	go func() {
		<-ctx.Done()
		sess.unblockSID()
		sess.closeEvents()
	}()

	return sess, nil
}

// startServe launches (or relaunches) the `opencode serve` child on a fresh
// port and starts a per-incarnation SSE loop bound to it. Called once by Spawn
// and again by SendPrompt after Interrupt() hibernated the serve. Each call
// installs a fresh serveExitDone channel and sseCancel so the previous
// incarnation's goroutines are fully superseded.
func (s *opencodeSession) startServe(ctx context.Context) error {
	// Pick a random free TCP port for the opencode server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("opencode: find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	serve := exec.CommandContext(ctx, "opencode", "serve", "--port", fmt.Sprint(port))
	serve.Dir = s.workdir
	serve.Env = buildEnv(s.env)
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		return fmt.Errorf("opencode serve: %w", err)
	}

	// One goroutine owns serve.Wait() for this incarnation; waitReady checks
	// exitDone non-blocking, Close()/Interrupt() read it blocking. Catches the
	// TOCTOU port-grab window between net.Listen() and opencode binding — if the
	// port was stolen, opencode exits ~immediately and we surface a clear error.
	exitDone := make(chan struct{})
	go func() {
		werr := serve.Wait()
		s.mu.Lock()
		s.serveExitErr = werr
		s.mu.Unlock()
		close(exitDone)
	}()

	s.mu.Lock()
	s.baseURL = baseURL
	s.serve = serve
	s.serveExitDone = exitDone
	s.mu.Unlock()

	if err := s.waitReady(ctx, baseURL, exitDone, 10*time.Second); err != nil {
		_ = serve.Process.Kill()
		<-exitDone
		return fmt.Errorf("opencode serve not ready: %w", err)
	}

	// Per-serve SSE loop. Its own cancel lets Interrupt()/Close() stop just this
	// incarnation without spinning on a dead port after the serve is killed; the
	// next relaunch starts a fresh loop bound to the new port.
	sseCtx, sseCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.sseCancel = sseCancel
	s.mu.Unlock()
	go s.sseLoop(sseCtx, baseURL)

	return nil
}

// waitReady polls /global/health until the serve subprocess is responsive,
// the deadline elapses, or the subprocess exits early.
func (s *opencodeSession) waitReady(ctx context.Context, baseURL string, exitDone chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := baseURL + "/global/health"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-exitDone:
			s.mu.Lock()
			werr := s.serveExitErr
			s.mu.Unlock()
			return fmt.Errorf("opencode serve exited during startup (port grab? missing binary?): %w", werr)
		default:
		}
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

type opencodeSession struct {
	// workdir/env/spawnCtx are captured at Spawn and reused to relaunch the
	// serve after Interrupt(): opencode persists session state to disk, so a
	// fresh serve resumes the same conversation via `--session <sid>`.
	workdir  string
	env      []string
	spawnCtx context.Context

	busy     atomic.Bool
	events   chan Event
	eventsMu sync.RWMutex // guards closed
	closed   bool

	// lifeMu serializes serve-lifecycle transitions — the SendPrompt() resume
	// swap, Interrupt(), and Close() — against each other. SendPrompt runs on
	// the chat-stream goroutine while Interrupt runs on the control-channel
	// goroutine (registry.InterruptSession), so without this the read-dormant →
	// startServe → clear-dormant sequence could interleave with Interrupt's
	// kill → set-dormant and clobber state / orphan a fresh serve. Held across
	// the (blocking) kill+wait and relaunch; never held with s.mu.
	lifeMu sync.Mutex

	// mu guards every field below: the serve incarnation (baseURL/serve/
	// serveExitErr/serveExitDone/sseCancel) is replaced across
	// Interrupt()+SendPrompt() restarts, and dormant/curRunCancel/sid are
	// touched from both the caller and the run goroutine.
	mu            sync.RWMutex
	sid           string
	baseURL       string
	serve         *exec.Cmd
	serveExitErr  error
	serveExitDone chan struct{}
	sseCancel     context.CancelFunc
	dormant       bool
	curRunCancel  context.CancelFunc

	sidCh     chan struct{}
	sidOnce   sync.Once
	resumeSID string
}

// emit safely sends ev to s.events; drops if the channel has been closed by
// closeEvents, preventing the closed-channel send panic.
func (s *opencodeSession) emit(ev Event) {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	if s.closed {
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

// closeEvents marks the session closed and shuts events down exactly once.
// Must be the sole caller of close(s.events).
func (s *opencodeSession) closeEvents() {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.events)
}

// unblockSID closes s.sidCh exactly once. Use to either publish a real sid
// (after also setting s.sid under s.mu) or release waiters when opencode
// exited before emitting session.created (SessionID() then returns "").
func (s *opencodeSession) unblockSID() {
	s.sidOnce.Do(func() {
		close(s.sidCh)
	})
}

func (s *opencodeSession) SessionID() string {
	<-s.sidCh
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sid
}

func (s *opencodeSession) Events() <-chan Event { return s.events }

func (s *opencodeSession) SendPrompt(ctx context.Context, text string) error {
	if !s.busy.CompareAndSwap(false, true) {
		s.emit(Event{Kind: EventError, Err: fmt.Errorf("busy: another prompt is running")})
		return nil
	}

	// If a prior Interrupt() hibernated the serve, bring it back before running.
	// opencode persisted the session to disk, so `--session <sid>` (appended
	// below) resumes the same conversation on the fresh serve. lifeMu makes the
	// read-dormant → startServe → clear-dormant swap atomic against a concurrent
	// Interrupt()/Close() (see lifeMu doc).
	s.lifeMu.Lock()
	s.mu.RLock()
	dormant := s.dormant
	s.mu.RUnlock()
	if dormant {
		if err := s.startServe(s.spawnCtx); err != nil {
			s.lifeMu.Unlock()
			s.busy.Store(false)
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode resume serve: %w", err)})
			return nil
		}
		s.mu.Lock()
		s.dormant = false
		s.mu.Unlock()
	}
	s.lifeMu.Unlock()

	// Derive a cancelable ctx for this run so Interrupt() can stop the client
	// cleanly: cancellation suppresses the run's exit error (see below) and the
	// goroutine still emits EventResult to close the turn.
	runCtx, runCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.curRunCancel = runCancel
	s.mu.Unlock()

	go func() {
		defer s.busy.Store(false)
		// Always release SessionID() waiters when this goroutine exits — if
		// opencode crashed before emitting session.created, sidCh would
		// otherwise stay open forever.
		defer s.unblockSID()
		defer func() {
			s.mu.Lock()
			s.curRunCancel = nil
			s.mu.Unlock()
			runCancel()
		}()

		s.mu.RLock()
		baseURL := s.baseURL
		args := []string{"run", "--attach", baseURL, "--format", "json"}
		if s.sid != "" {
			args = append(args, "--session", s.sid)
		} else if s.resumeSID != "" {
			args = append(args, "--session", s.resumeSID)
		}
		s.mu.RUnlock()
		args = append(args, text)

		cmd := exec.CommandContext(runCtx, "opencode", args...)
		cmd.Dir = s.workdir
		cmd.Env = buildEnv(nil)
		cmd.Stderr = os.Stderr

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: stdout pipe: %w", err)})
			return
		}
		if err := cmd.Start(); err != nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: start: %w", err)})
			return
		}

		// Parse run output to capture session ID and detect step_finish.
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			var ev struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if ev.SessionID != "" {
				s.sidOnce.Do(func() {
					s.mu.Lock()
					s.sid = ev.SessionID
					s.mu.Unlock()
					close(s.sidCh)
				})
			}
		}
		if err := sc.Err(); err != nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: stdout scan: %w", err)})
		}
		// A non-zero exit that is the result of an Interrupt() (runCtx cancelled)
		// or session teardown (ctx cancelled) is expected — don't surface it.
		if err := cmd.Wait(); err != nil && ctx.Err() == nil && runCtx.Err() == nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run exited: %w", err)})
		}
		// Emit EventResult after opencode run exits (or is interrupted) — closes
		// the turn for the consumer. More reliable than the session.idle SSE
		// (which can fire spuriously before text is done).
		s.emit(Event{Kind: EventResult, Text: "stop"})
	}()
	return nil
}

// Interrupt stops the in-flight turn for an opencode session. Unlike cc — which
// writes a stream-json control_request on a long-lived stdin (see
// ccSession.Interrupt) — opencode's generation is owned by the persistent
// `opencode serve` process and streamed over SSE independently of the
// short-lived `opencode run` client. Killing the run subprocess therefore does
// NOT stop generation: the serve keeps producing tokens and the SSE stream
// keeps flowing (verified empirically, tether#11 — killing the run process,
// even its whole process group, left the serve generating). The only effective
// interrupt is to kill the serve, which owns the work.
//
// The session stays resumable: opencode persists session state to disk, so the
// next SendPrompt relaunches a fresh serve (the dormant path in SendPrompt) and
// continues the same conversation via `--session <sid>`.
func (s *opencodeSession) Interrupt() error {
	// Nothing running -> nothing to interrupt (avoid needlessly hibernating an
	// idle serve).
	if !s.busy.Load() {
		return nil
	}

	// Serialize against a concurrent SendPrompt-resume / Close (see lifeMu doc).
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	s.mu.Lock()
	runCancel := s.curRunCancel
	sseCancel := s.sseCancel
	serve := s.serve
	exitDone := s.serveExitDone
	s.dormant = true
	s.mu.Unlock()

	// 1. Cancel the in-flight `opencode run` client so its goroutine winds down
	//    without surfacing a spurious exit error and emits EventResult.
	if runCancel != nil {
		runCancel()
	}
	// 2. Stop this serve's SSE loop so it doesn't spin reconnecting to a port
	//    that is about to die; SendPrompt starts a fresh loop on relaunch.
	if sseCancel != nil {
		sseCancel()
	}
	// 3. Kill the serve — it owns generation, so this actually stops it.
	if serve != nil && serve.Process != nil {
		_ = serve.Process.Kill()
		if exitDone != nil {
			<-exitDone
		}
	}
	return nil
}

func (s *opencodeSession) Close() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	s.mu.Lock()
	runCancel := s.curRunCancel
	sseCancel := s.sseCancel
	serve := s.serve
	exitDone := s.serveExitDone
	s.mu.Unlock()

	// Cancel any in-flight `opencode run` client too: killing the serve alone
	// may not make an attached run client exit, which would block its goroutine
	// on cmd.Wait() forever (goroutine leak + busy stuck true).
	if runCancel != nil {
		runCancel()
	}
	if sseCancel != nil {
		sseCancel()
	}
	if serve != nil && serve.Process != nil {
		_ = serve.Process.Kill()
	}
	if exitDone != nil {
		<-exitDone
	}
	s.closeEvents()

	s.mu.RLock()
	err := s.serveExitErr
	s.mu.RUnlock()
	return err
}

// sseLoop consumes the serve's SSE /global/event stream for one serve
// incarnation, bound to baseURL and stopped via ctx (see startServe). It does
// NOT close the session's event stream on exit — the session outlives any
// single serve incarnation (Interrupt() kills one, SendPrompt relaunches
// another); closeEvents is owned by Close() / the Spawn ctx-done goroutine.
func (s *opencodeSession) sseLoop(ctx context.Context, baseURL string) {
	client := &http.Client{}
	url := baseURL + "/global/event"

	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.readSSE(ctx, resp.Body, s.emit)
		resp.Body.Close()

		if ctx.Err() != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type ssePayload struct {
	Payload struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	} `json:"payload"`
}

func (s *opencodeSession) readSSE(ctx context.Context, r io.Reader, emit func(Event)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var wrapper ssePayload
		if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
			continue
		}

		switch wrapper.Payload.Type {
		case "message.part.delta":
			var props struct {
				SessionID string `json:"sessionID"`
				Field     string `json:"field"`
				Delta     string `json:"delta"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err != nil {
				continue
			}
			if props.Field == "text" && props.Delta != "" {
				if props.SessionID != "" {
					s.sidOnce.Do(func() {
						s.mu.Lock()
						s.sid = props.SessionID
						s.mu.Unlock()
						close(s.sidCh)
					})
				}
				emit(Event{Kind: EventText, Text: props.Delta})
			}

		case "session.created":
			var props struct {
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err != nil {
				continue
			}
			if props.SessionID != "" {
				s.sidOnce.Do(func() {
					s.mu.Lock()
					s.sid = props.SessionID
					s.mu.Unlock()
					close(s.sidCh)
				})
				emit(Event{Kind: EventInit, SessionID: props.SessionID})
			}

		case "session.error":
			var props struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err == nil {
				if props.Error.Message != "" {
					emit(Event{Kind: EventError, Err: errors.New(props.Error.Message)})
				}
			}
		}
	}
}
