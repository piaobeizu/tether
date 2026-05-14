package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
)

// OpenCodeProvider implements AgentProvider for opencode (D-17a A2 per-prompt spawn).
type OpenCodeProvider struct{}

func NewOpenCodeProvider() *OpenCodeProvider { return &OpenCodeProvider{} }

func (p *OpenCodeProvider) Name() string { return "opencode" }

func (p *OpenCodeProvider) Spawn(_ context.Context, cfg SpawnConfig) (Session, error) {
	s := &opencodeSession{
		events: make(chan Event, 64),
		sidCh:  make(chan struct{}),
	}
	if cfg.ResumeSessionID != "" {
		s.sid = cfg.ResumeSessionID
		close(s.sidCh) // already known — unblock SessionID() immediately
	}
	return s, nil
}

type opencodeSession struct {
	mu     sync.RWMutex // protects sid
	busy   atomic.Bool
	sid    string
	sidCh  chan struct{} // closed once sid is first set; unblocks SessionID()
	events chan Event
}

// SessionID blocks until the opencode session ID is known (set from the first
// JSON event in SendPrompt). This mirrors ClaudeCodeProvider's blocking
// behaviour so the registry re-key goroutine never writes "" as a key.
func (s *opencodeSession) SessionID() string {
	<-s.sidCh
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sid
}

func (s *opencodeSession) Events() <-chan Event {
	return s.events
}

func (s *opencodeSession) SendPrompt(ctx context.Context, text string) error {
	if !s.busy.CompareAndSwap(false, true) {
		select {
		case s.events <- Event{Kind: EventError, Err: fmt.Errorf("busy: another prompt is running")}:
		default:
		}
		return nil
	}
	defer s.busy.Store(false)

	args := []string{"run", "--format", "json"}
	s.mu.RLock()
	if s.sid != "" {
		args = append(args, "-s", s.sid)
	}
	s.mu.RUnlock()
	args = append(args, text)

	cmd := exec.CommandContext(ctx, "opencode", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.emitError(fmt.Sprintf("opencode pipe: %v", err))
		return nil
	}
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		s.emitError(fmt.Sprintf("opencode start: %v", err))
		return nil
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			slog.Debug("opencode stderr", "line", sc.Text())
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<16), 1<<20) // max 1MB per line
	for sc.Scan() {
		line := sc.Bytes()
		var ev struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionID"`
			Part      struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"part"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			slog.Warn("opencode: JSON parse error", "line", string(line), "err", err)
			s.emitError(fmt.Sprintf("parse error: %v | raw: %.120s", err, line))
			continue
		}
		if ev.SessionID != "" {
			s.mu.Lock()
			if s.sid == "" {
				s.sid = ev.SessionID
				close(s.sidCh) // unblock SessionID() waiters
			}
			s.mu.Unlock()
		}
		if ev.Type == "text" && ev.Part.Text != "" {
			s.events <- Event{Kind: EventText, Text: ev.Part.Text}
		}
	}
	if err := sc.Err(); err != nil {
		s.emitError(fmt.Sprintf("opencode stream truncated: %v", err))
	}
	if err := cmd.Wait(); err != nil {
		s.emitError(fmt.Sprintf("opencode exit: %v", err))
	}
	// Signal turn complete so the browser clears the streaming indicator.
	select {
	case s.events <- Event{Kind: EventResult, Text: "stop"}:
	default:
	}
	return nil
}

func (s *opencodeSession) emitError(msg string) {
	select {
	case s.events <- Event{Kind: EventError, Err: errors.New(msg)}:
	default:
	}
}

func (s *opencodeSession) Interrupt() error { return nil }
func (s *opencodeSession) Close() error      { return nil }
