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
		ctx:       ctx,
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
	ctx      context.Context // Spawn ctx; escape for the blocking terminal-event send
	stdin    interface{ Close() error }
	enc      *json.Encoder
	events   chan Event
	mu       sync.Mutex
	sid      string
	sidReady chan struct{}
	sidOnce  sync.Once
	reqSeq   int // control_request id counter (T9 pause/interrupt), guarded by mu
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

// Interrupt aborts the in-flight turn WITHOUT killing the cc subprocess
// (tether#8 T9). Earlier this sent SIGINT to s.cmd.Process, which cc treats
// like a Ctrl-C from a real terminal — acceptable for a bare `claude` REPL,
// but tether drives cc with `--input-format stream-json` and holds its
// stdin open across turns (see Spawn/SendPrompt above), so the process is
// meant to stay alive and resumable across the whole session lifetime, not
// just the current turn. Instead we write a stream-json control_request on
// the same mu-guarded encoder SendPrompt uses:
//
//	{"type":"control_request","request_id":"<unique>","request":{"subtype":"interrupt"}}
//
// cc aborts the current turn and stays running; the caller (Registry.
// InterruptSession) treats the session as immediately resumable via a
// subsequent SendPrompt — no respawn, no --resume flag needed.
//
// request_id only needs to be unique per-session (cc doesn't require a
// global namespace); a mutex-guarded counter avoids pulling in time/rand
// for something this local. We do not wait for the matching
// control_response — this is fire-and-remember, like SendPrompt; readLoop
// (see parseLine) recognizes and drops the control_response line so it
// never surfaces as a spurious event.
//
// VERIFY: this shape is confirmed against tether v1's production
// implementation (v0/internal/backend/claude/message.go
// OutboundControlRequest + session.go SendInterrupt, which shipped this
// exact interrupt flow), not against upstream `claude` CLI docs directly —
// re-confirm against the pinned cc version if this ever misbehaves.
func (s *ccSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqSeq++
	req := map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("tether-interrupt-%d", s.reqSeq),
		"request": map[string]any{
			"subtype": "interrupt",
		},
	}
	return s.enc.Encode(req)
}

func (s *ccSession) Close() error {
	_ = s.stdin.Close()
	return s.cmd.Wait()
}

// emit sends ev to s.events. Terminal events (isTerminal) block until delivered
// so a full buffer can't drop them — a lost EventResult/EventError leaves the
// consumer's turn open forever (tether#14). fanOut drains Events() until close,
// so the send always makes progress; s.ctx is the escape when the session is
// torn down (client disconnect cancels ctx, which also kills the subprocess).
// readLoop is the sole sender and closes s.events only after it returns, so
// there is no concurrent send/close here. Non-terminal events (token deltas
// etc.) keep the non-blocking backpressure drop.
func (s *ccSession) emit(ev Event) {
	if isTerminal(ev.Kind) {
		select {
		case s.events <- ev:
		case <-s.ctx.Done():
		}
		return
	}
	select {
	case s.events <- ev:
	default:
	}
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
		s.emit(*ev)
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
// lines when --include-partial-messages is on. We care about the
// content_block_delta variants with delta.type=="text_delta" (assistant text)
// and "thinking_delta" (extended-thinking tokens, tether#34).
type rawPartialMessage struct {
	Type  string `json:"type"` // "content_block_delta", "message_start", etc.
	Delta struct {
		Type     string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta", "signature_delta"
		Text     string `json:"text"`
		Thinking string `json:"thinking"` // populated when Type=="thinking_delta"
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
	// We forward text_delta as EventText and thinking_delta as EventThinking
	// (tether#34). Everything else is skipped: signature deltas for thinking
	// blocks carry no user-visible content; partial JSON for tool_use args is
	// handled via the final `assistant` event below, which carries the
	// complete input.
	if raw.Type == "stream_event" && raw.Event != nil {
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "text_delta" &&
			raw.Event.Delta.Text != "" {
			return &Event{Kind: EventText, Text: raw.Event.Delta.Text}
		}
		// Extended-thinking tokens (tether#34). Forwarded as EventThinking;
		// registry.fanOut routes it through translateEvent (bypassing the
		// fence parser), so it is broadcast to the browser but never
		// accumulated into assistant history — thinking stays ephemeral.
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "thinking_delta" &&
			raw.Event.Delta.Thinking != "" {
			return &Event{Kind: EventThinking, Text: raw.Event.Delta.Thinking}
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

	// control_response is cc's reply to a control_request WE sent (currently
	// only the T9 interrupt request from ccSession.Interrupt). It carries no
	// user-visible content and Interrupt() doesn't correlate/await it, so
	// this is intentionally a no-op — matched explicitly (rather than
	// relying on the catch-all `return nil` below) so a future contributor
	// adding a new fallthrough Event doesn't accidentally turn cc's ack into
	// a spurious EventError/bad event in the chat stream.
	if raw.Type == "control_response" {
		return nil
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
