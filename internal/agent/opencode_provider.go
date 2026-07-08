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
	// Pick a random free TCP port for the opencode server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("opencode: find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	workdir := cfg.Workdir
	if workdir == "" {
		workdir, _ = os.Getwd()
	}

	serve := exec.CommandContext(ctx, "opencode", "serve", "--port", fmt.Sprint(port))
	serve.Dir = workdir
	serve.Env = buildEnv(cfg.Env)
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		return nil, fmt.Errorf("opencode serve: %w", err)
	}

	sess := &opencodeSession{
		baseURL:       baseURL,
		serve:         serve,
		workdir:       workdir,
		events:        make(chan Event, 64),
		sidCh:         make(chan struct{}),
		serveExitDone: make(chan struct{}),
		resumeSID:     cfg.ResumeSessionID,
	}
	// Single goroutine owns serve.Wait(); waitReady checks done non-blocking,
	// Close() reads done blocking. Catches the TOCTOU port-grab window
	// between net.Listen() and opencode binding — if the port was stolen,
	// opencode exits ~immediately and we get a clear error instead of
	// "timeout after 10s".
	go func() {
		err := serve.Wait()
		sess.serveExitErr = err
		close(sess.serveExitDone)
	}()

	if err := sess.waitReady(ctx, 10*time.Second); err != nil {
		_ = serve.Process.Kill()
		<-sess.serveExitDone
		return nil, fmt.Errorf("opencode serve not ready: %w", err)
	}

	go sess.sseLoop(ctx)
	return sess, nil
}

// waitReady polls /global/health until the serve subprocess is responsive,
// the deadline elapses, or the subprocess exits early.
func (s *opencodeSession) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := s.baseURL + "/global/health"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-s.serveExitDone:
			return fmt.Errorf("opencode serve exited during startup (port grab? missing binary?): %w", s.serveExitErr)
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
	baseURL  string
	serve    *exec.Cmd
	workdir  string
	busy     atomic.Bool
	events   chan Event
	eventsMu sync.RWMutex // guards closed
	closed   bool
	mu       sync.RWMutex
	sid      string
	sidCh    chan struct{}
	sidOnce  sync.Once
	// Set once by the serve-monitor goroutine right before serveExitDone
	// closes; safe to read after <-serveExitDone without a lock.
	serveExitErr  error
	serveExitDone chan struct{}
	resumeSID     string
}

// emit safely sends ev to s.events; drops if the channel has been closed by
// sseLoop's defer, preventing the closed-channel send panic.
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

	go func() {
		defer s.busy.Store(false)
		// Always release SessionID() waiters when this goroutine exits — if
		// opencode crashed before emitting session.created, sidCh would
		// otherwise stay open forever.
		defer s.unblockSID()

		args := []string{"run", "--attach", s.baseURL, "--format", "json"}
		s.mu.RLock()
		if s.sid != "" {
			args = append(args, "--session", s.sid)
		} else if s.resumeSID != "" {
			args = append(args, "--session", s.resumeSID)
		}
		s.mu.RUnlock()
		args = append(args, text)

		cmd := exec.CommandContext(ctx, "opencode", args...)
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
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run exited: %w", err)})
		}
		// Emit EventResult after opencode run exits — more reliable than
		// session.idle SSE (which can fire spuriously before text is done).
		s.emit(Event{Kind: EventResult, Text: "stop"})
	}()
	return nil
}

// Interrupt is a documented no-op for opencode (tether#8 T9 targets cc's
// stream-json control_request interrupt only — see ccSession.Interrupt in
// claude_provider.go). opencode's `serve` HTTP backend has no equivalent
// interrupt call wired here, so a DAG-card pause click against an opencode
// session currently does nothing observable. This asymmetry is intentional
// scope for T9, not an oversight — extending pause to opencode is future
// work, not tracked as a wi here.
func (s *opencodeSession) Interrupt() error { return nil }

func (s *opencodeSession) Close() error {
	if s.serve.Process != nil {
		_ = s.serve.Process.Kill()
	}
	<-s.serveExitDone
	return s.serveExitErr
}

func (s *opencodeSession) sseLoop(ctx context.Context) {
	defer s.closeEvents()
	// Also unblock SessionID() waiters on shutdown — sseLoop is the goroutine
	// that publishes the real sid via session.created; without this fallback,
	// a waiter on s.sidCh would hang forever if the loop exits before sid.
	defer s.unblockSID()

	client := &http.Client{}
	url := s.baseURL + "/global/event"

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
				Error struct{ Message string `json:"message"` } `json:"error"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err == nil {
				if props.Error.Message != "" {
					emit(Event{Kind: EventError, Err: errors.New(props.Error.Message)})
				}
			}
		}
	}
}
