package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
)

// ClaudeCodeProvider implements AgentProvider for `claude` CLI (D-05a §4).
type ClaudeCodeProvider struct {
	ccPath string
}

// NewClaudeCodeProvider creates a provider using the given cc binary path.
func NewClaudeCodeProvider(ccPath string) *ClaudeCodeProvider {
	return &ClaudeCodeProvider{ccPath: ccPath}
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) Spawn(ctx context.Context, cfg SpawnConfig) (Session, error) {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// Emit token-level text deltas via `stream_event` lines (D-05a §3).
		// Without this, `assistant` arrives as one complete block, which the UI
		// renders as a sudden full-message reveal instead of streaming.
		"--include-partial-messages",
		// Force "default" permission mode so PreToolUse hooks ALWAYS fire,
		// regardless of user's ~/.claude/settings.json `defaultMode` setting.
		// Without this, users with `defaultMode: "auto"` or `bypassPermissions`
		// silently skip tether's permission UI.
		"--permission-mode", "default",
	}
	if cfg.ResumeSessionID != "" {
		args = append(args, "--resume", cfg.ResumeSessionID)
	}

	cmd := exec.CommandContext(ctx, p.ccPath, args...)
	cmd.Env = buildEnv(cfg.Env)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cc: %w", err)
	}

	enc := json.NewEncoder(stdin)
	enc.SetEscapeHTML(false)

	sess := &ccSession{
		cmd:       cmd,
		stdin:     stdin,
		enc:       enc,
		events:    make(chan Event, 64),
		sidReady:  make(chan struct{}),
	}
	go sess.readLoop(bufio.NewScanner(stdout))
	return sess, nil
}

// buildEnv constructs the subprocess environment. IS_SANDBOX=1 is injected
// when running as root (D-05a §2 fact 5). Extra env vars are appended after.
func buildEnv(extra []string) []string {
	env := os.Environ()
	if os.Geteuid() == 0 {
		env = append(env, "IS_SANDBOX=1")
	}
	return append(env, extra...)
}

// ccSession is a live ClaudeCode stream-json session.
type ccSession struct {
	cmd      *exec.Cmd
	stdin    interface{ Close() error }
	enc      *json.Encoder
	events   chan Event
	mu       sync.Mutex
	sid      string
	sidReady chan struct{}
	sidOnce  sync.Once
}

func (s *ccSession) SessionID() string {
	<-s.sidReady
	return s.sid
}

func (s *ccSession) SendPrompt(_ context.Context, text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(msg)
}

func (s *ccSession) Events() <-chan Event { return s.events }

func (s *ccSession) Interrupt() error {
	if s.cmd.Process == nil {
		return nil
	}
	return s.cmd.Process.Signal(os.Interrupt)
}

func (s *ccSession) Close() error {
	_ = s.stdin.Close()
	return s.cmd.Wait()
}

// readLoop consumes stream-json lines from cc stdout and emits Events.
func (s *ccSession) readLoop(scanner *bufio.Scanner) {
	scanner.Buffer(make([]byte, 1<<20), 100<<20)
	defer close(s.events)

	for scanner.Scan() {
		ev := s.parseLine(scanner.Bytes())
		if ev == nil {
			continue
		}
		select {
		case s.events <- *ev:
		default:
		}
	}
}

// rawStreamEvent is the top-level stream-json event shape (D-05a §3).
type rawStreamEvent struct {
	Type      string             `json:"type"`
	Subtype   string             `json:"subtype,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	Message   *rawAssistMsg      `json:"message,omitempty"`
	Result    string             `json:"result,omitempty"`
	Event     *rawPartialMessage `json:"event,omitempty"` // populated when type=="stream_event"
}

type rawAssistMsg struct {
	Content []rawContentBlock `json:"content"`
}

type rawContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// rawPartialMessage is the Anthropic-native SSE event embedded in stream_event
// lines when --include-partial-messages is on. We only care about the
// content_block_delta variant with delta.type=="text_delta".
type rawPartialMessage struct {
	Type  string `json:"type"` // "content_block_delta", "message_start", etc.
	Delta struct {
		Type string `json:"type"` // "text_delta", "input_json_delta", "signature_delta"
		Text string `json:"text"`
	} `json:"delta"`
}

func (s *ccSession) parseLine(line []byte) *Event {
	var raw rawStreamEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	// system/init — captures SessionID once; subsequent inits per turn are
	// metadata refreshes only, NOT new-session boundaries (D-05a §2 fact 2).
	if raw.Type == "system" && raw.Subtype == "init" && raw.SessionID != "" {
		s.sidOnce.Do(func() {
			s.sid = raw.SessionID
			close(s.sidReady)
		})
		return &Event{Kind: EventInit, SessionID: raw.SessionID}
	}

	// stream_event lines carry token-level deltas (--include-partial-messages).
	// We forward text_delta as EventText and skip everything else (signature
	// deltas for thinking blocks; partial JSON for tool_use args is handled
	// via the final `assistant` event below, which carries the complete input).
	if raw.Type == "stream_event" && raw.Event != nil {
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "text_delta" &&
			raw.Event.Delta.Text != "" {
			return &Event{Kind: EventText, Text: raw.Event.Delta.Text}
		}
		return nil
	}

	// `assistant` events arrive after all deltas have streamed. Text blocks are
	// redundant (already streamed via stream_event); only tool_use blocks carry
	// information we haven't emitted yet.
	if raw.Type == "assistant" && raw.Message != nil {
		for _, block := range raw.Message.Content {
			if block.Type == "tool_use" {
				// tool_use is a content block, NOT a top-level event (D-05a §3, Risk #4).
				return &Event{
					Kind: EventToolUse,
					ToolUse: &ToolUseEvent{
						ID:    block.ID,
						Name:  block.Name,
						Input: block.Input,
					},
				}
			}
		}
		return nil
	}

	if raw.Type == "result" {
		return &Event{Kind: EventResult, Text: raw.Result}
	}

	if raw.Type == "rate_limit_event" {
		return &Event{Kind: EventRateLimit}
	}

	return nil
}

// Stub providers for future providers (D-17a §5).

// CursorProvider — stub; see D-17a §5.2.
type CursorProvider struct{}

func (CursorProvider) Name() string                                { return "cursor" }
func (CursorProvider) Spawn(_ context.Context, _ SpawnConfig) (Session, error) {
	return nil, fmt.Errorf("cursor provider: not yet implemented")
}
