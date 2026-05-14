package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	if err := waitReady(ctx, baseURL+"/global/health", 10*time.Second); err != nil {
		_ = serve.Process.Kill()
		return nil, fmt.Errorf("opencode serve not ready: %w", err)
	}

	sess := &opencodeSession{
		baseURL:   baseURL,
		serve:     serve,
		workdir:   workdir,
		events:    make(chan Event, 64),
		sidCh:     make(chan struct{}),
		resumeSID: cfg.ResumeSessionID,
	}
	go sess.sseLoop(ctx)
	return sess, nil
}

func waitReady(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
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
	baseURL   string
	serve     *exec.Cmd
	workdir   string
	busy      atomic.Bool
	events    chan Event
	mu        sync.RWMutex
	sid       string
	sidCh     chan struct{}
	sidOnce   sync.Once
	resumeSID string
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
		select {
		case s.events <- Event{Kind: EventError, Err: fmt.Errorf("busy: another prompt is running")}:
		default:
		}
		return nil
	}

	go func() {
		defer s.busy.Store(false)

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
			slog.Warn("opencode run: stdout pipe", "err", err)
			return
		}
		if err := cmd.Start(); err != nil {
			slog.Warn("opencode run: start", "err", err)
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
		_ = cmd.Wait()
		// Emit EventResult after opencode run exits — more reliable than
		// session.idle SSE (which can fire spuriously before text is done).
		select {
		case s.events <- Event{Kind: EventResult, Text: "stop"}:
		default:
		}
	}()
	return nil
}

func (s *opencodeSession) Interrupt() error { return nil }

func (s *opencodeSession) Close() error {
	if s.serve.Process != nil {
		_ = s.serve.Process.Kill()
	}
	return s.serve.Wait()
}

func (s *opencodeSession) sseLoop(ctx context.Context) {
	defer close(s.events)

	emit := func(ev Event) {
		select {
		case s.events <- ev:
		default:
		}
	}

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
		s.readSSE(ctx, resp.Body, emit)
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
