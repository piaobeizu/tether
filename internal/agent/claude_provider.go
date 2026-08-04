package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// ClaudeCodeProvider implements AgentProvider for `claude` CLI (D-05a §4).
type ClaudeCodeProvider struct {
	ccPath string
	// stderr is where the cc subprocess's stderr goes. nil — the only state
	// production ever puts it in, since NewClaudeCodeProvider takes no options
	// at its single call site (internal/server/lifecycle.go) — resolves to
	// os.Stderr in stderrSink, i.e. byte-for-byte the pre-seam behaviour.
	// Redirecting it is a test-only affordance (see withStderr).
	stderr io.Writer
}

// ccOption configures a ClaudeCodeProvider at construction time. The type is
// unexported on purpose: the seam exists so this package's tests can observe a
// spawned process, NOT so an embedder (or an environment variable) can
// reconfigure the daemon's agent at runtime (tether#53).
type ccOption func(*ClaudeCodeProvider)

// withStderr routes the cc subprocess's stderr to w instead of the daemon's own
// os.Stderr, so a test can assert on what cc wrote there — notably the
// "No conversation found with session ID: <uuid>" line a failed `--resume`
// produces, which is otherwise unobservable from inside the process.
func withStderr(w io.Writer) ccOption {
	return func(p *ClaudeCodeProvider) { p.stderr = w }
}

// NewClaudeCodeProvider creates a provider using the given cc binary path.
// Options are package-visible only; passing none (what production does) leaves
// every field at the behaviour that predates the options parameter.
func NewClaudeCodeProvider(ccPath string, opts ...ccOption) *ClaudeCodeProvider {
	p := &ClaudeCodeProvider{ccPath: ccPath}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// stderrSink resolves the subprocess stderr destination. An unset (nil) sink
// means os.Stderr, so a zero-value ClaudeCodeProvider literal behaves exactly
// like one built before the seam existed.
func (p *ClaudeCodeProvider) stderrSink() io.Writer {
	if p.stderr == nil {
		return os.Stderr
	}
	return p.stderr
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) Spawn(ctx context.Context, cfg SpawnConfig) (Session, error) {
	// ⑧ (mem_2ruSlrHR, measured on 2.1.220): cc rejects --session-id together
	// with --resume unless --fork-session is also passed, exiting 1 before it
	// emits anything. Caught here rather than downstream because that exit looks
	// EXACTLY like a failed resume (no init, SessionID() == ""), so the caller's
	// fallback would quietly paper over the bug by spawning a fresh session and
	// the operator would only see "context was lost, sometimes". Failing the
	// spawn names the real cause.
	if cfg.SessionID != "" && cfg.ResumeSessionID != "" {
		return nil, fmt.Errorf(
			"spawn cc: SessionID (%s) and ResumeSessionID (%s) are mutually exclusive: "+
				"--session-id is fresh-spawn-only, reconnect passes --resume alone",
			cfg.SessionID, cfg.ResumeSessionID)
	}

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
	// Exactly one of these can be set (guarded above). Reconnect resumes an
	// existing transcript; a fresh spawn pins the id the daemon minted so the
	// tether sid, the cc sid and the on-disk transcript name are the same string
	// from the very first byte — no "listen for init and hope" step (tether#50).
	switch {
	case cfg.ResumeSessionID != "":
		args = append(args, "--resume", cfg.ResumeSessionID)
	case cfg.SessionID != "":
		args = append(args, "--session-id", cfg.SessionID)
	}

	cmd := exec.CommandContext(ctx, p.ccPath, args...)
	// cc's on-disk conversation and file edits are cwd-scoped, so the
	// subprocess must run in the workspace directory, not wherever the
	// daemon happened to start (tether#51).
	cmd.Dir = ResolveWorkdir(cfg.Workdir)
	cmd.Env = buildEnv(cfg.Env)
	cmd.Stderr = p.stderrSink()

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
		cmd:      cmd,
		ctx:      ctx,
		stdin:    stdin,
		enc:      enc,
		events:   make(chan Event, 64),
		sidReady: make(chan struct{}),
		done:     make(chan struct{}),
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
	done     chan struct{} // closed when readLoop returns (cc process fully exited)
	reqSeq   int           // control_request id counter (T9 pause/interrupt), guarded by mu
	// closeOnce/closeErr make Close idempotent — see Close for why it has to be.
	closeOnce sync.Once
	closeErr  error
}

// SessionID blocks until cc emits system/init (which resolves s.sid) OR the
// session dies first — a cc that exits before ever emitting init (e.g. a failed
// `--resume`, or a fresh cc that crashes on startup) would otherwise park this
// caller forever, since sidReady never closes. Selecting on `done` too lets it
// return "" on premature death so the caller can surface an error / spawn fresh
// instead of hanging the turn (tether#49).
func (s *ccSession) SessionID() string {
	select {
	case <-s.sidReady:
	case <-s.done:
	}
	return s.sid
}

// Alive reports whether the cc subprocess is still reading its stdin, by polling
// the same `done` channel SessionID() selects on (tether#49). No new state: done
// is closed by readLoop's own defer, i.e. when readLoop has stopped scanning cc's
// stdout — normally because the pipe EOF'd (the process is gone and the stdin
// this session writes prompts to has no reader), and also on a scanner error such
// as a line past the 100MB cap, which is equally terminal for this session
// because nothing restarts readLoop.
//
// The poll cannot block: a select with a default over an already-created channel
// touches no lock, no syscall and no I/O, so it answers in the caller's own
// goroutine no matter what state cc is in — the property tether#55 needs, since
// this runs on the reconnect path whose failure mode is a hang.
//
// One consequence worth stating rather than discovering: done closes when
// readLoop NOTICES the exit, not at the instant of exit, so there is a
// scheduler-wakeup-sized window in which a dead cc still reports Alive. That
// window is inherent to any non-blocking answer and is not the window tether#55
// is about — the reported one is between done closing (readLoop returned) and
// Registry.fanOut's deferred evict draining the buffered events, which this
// closes completely because done closes BEFORE events (defer order in readLoop:
// close(events) is registered first, so it runs last). That defer order is a
// hand-checked invariant, NOT one the tests pin — swapping the two defers leaves
// the suite green, because the two closes are adjacent enough that an observer
// almost never lands between them. Keep them in that order anyway: it is what
// makes "Events() closed ⟹ Alive() already false" true for fanOut.
func (s *ccSession) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
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

// Close reaps the cc subprocess. It closes the stdin this session writes prompts
// to and then waits for the process, which is what releases the three things a
// session otherwise leaves behind for the whole life of the daemon (tether#56):
// the `[claude] <defunct>` zombie, the stdin fd, and the goroutine
// exec.CommandContext starts to watch ctx — that one parks on an internal
// unbuffered channel whose only receiver is inside Wait, so with no cmd.WaitDelay
// set (tether sets none) it is Wait, and nothing else, that lets it finish. Note
// that a ctx cancellation does NOT release it on its own; it merely moves it from
// one park to the next.
//
// # Only safe once every read of cc's stdout has finished
//
// os/exec documents cmd.Wait as incorrect to call while a read from StdoutPipe
// is still in flight — Wait closes that pipe, out from under whoever is reading
// it. readLoop is the sole reader, so each caller has to establish that readLoop
// has stopped scanning BEFORE calling this. The two production callers do, by
// different routes, and both spell the argument out at the call site:
//
//   - Registry.teardown (internal/session/registry.go), from fanOut's defer:
//     Events() has closed and drained, and close(s.events) is readLoop's LAST
//     deferred statement.
//   - Attachment.resolve (internal/session/attach.go), on the failed-resume
//     path: SessionID() returned "" only because `done` closed, and
//     close(s.done) is likewise a readLoop defer.
//
// A third caller owes the same argument. "The process looks dead" is NOT it:
// readLoop can still be draining bytes the pipe buffered before the process
// exited, which is exactly the window the os/exec warning is about.
//
// # Idempotent on purpose
//
// The two callers above are not mutually exclusive — a failed resume reaps
// eagerly from the serveChat goroutine while that same entry's fanOut is
// independently unwinding toward its teardown defer — and cmd.Wait is not
// re-entrant: a second call answers "exec: Wait was already called" and cannot
// re-report the exit status. Guarding it here rather than asking every caller to
// coordinate keeps the reap at exactly one per session and gives every caller
// the same answer, instead of handing the loser an error about bookkeeping it
// did not do.
func (s *ccSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		s.closeErr = s.cmd.Wait()
	})
	return s.closeErr
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
	// Unblock any SessionID() waiter when the process exits — critical for a cc
	// that dies before emitting system/init (tether#49), where sidReady never
	// closes.
	defer close(s.done)

	for scanner.Scan() {
		// A single stream-json line can yield MULTIPLE events — e.g. a `user`
		// event batching the parallel tool_results for several tool_use blocks
		// (tether#38). Emit each so none is dropped.
		for _, ev := range s.parseLine(scanner.Bytes()) {
			s.emit(ev)
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
	Usage     *rawUsage          `json:"usage,omitempty"` // populated on the `result` event (tether#48)
	Event     *rawPartialMessage `json:"event,omitempty"` // populated when type=="stream_event"
}

// rawUsage is cc's per-turn token accounting on the `result` event. cc also
// reports cache_creation_input_tokens / cache_read_input_tokens and
// total_cost_usd here, but tether#48 surfaces only the plain input/output
// counts (owner scope: in/out tokens, no cache detail, no cost).
type rawUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
	// tool_result block fields (tether#38), present on a `user` event's content[].
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
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

// parseLine decodes one stream-json line into zero or more events. It returns a
// slice (not a single *Event) because a `user` / `assistant` message can carry
// several tool_result / tool_use blocks in one line (parallel tool calls), all
// of which must surface (tether#38). nil/empty = nothing to forward.
func (s *ccSession) parseLine(line []byte) []Event {
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
		return []Event{{Kind: EventInit, SessionID: raw.SessionID}}
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
			return []Event{{Kind: EventText, Text: raw.Event.Delta.Text}}
		}
		// Extended-thinking tokens (tether#34). Forwarded as EventThinking;
		// registry.fanOut routes it through translateEvent (bypassing the
		// fence parser), so it is broadcast to the browser but never
		// accumulated into assistant history — thinking stays ephemeral.
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "thinking_delta" &&
			raw.Event.Delta.Thinking != "" {
			return []Event{{Kind: EventThinking, Text: raw.Event.Delta.Thinking}}
		}
		return nil
	}

	// `assistant` events arrive after all deltas have streamed. Text blocks are
	// redundant (already streamed via stream_event); only tool_use blocks carry
	// information we haven't emitted yet. An assistant message can hold several
	// tool_use blocks (parallel tool calls) — emit one event per block.
	if raw.Type == "assistant" && raw.Message != nil {
		var evs []Event
		for _, block := range raw.Message.Content {
			if block.Type == "tool_use" {
				// tool_use is a content block, NOT a top-level event (D-05a §3, Risk #4).
				evs = append(evs, Event{
					Kind: EventToolUse,
					ToolUse: &ToolUseEvent{
						ID:    block.ID,
						Name:  block.Name,
						Input: block.Input,
					},
				})
			}
		}
		return evs
	}

	// `user` events carry tool_result blocks — the output of a tool cc ran
	// (tether#38). tool_result is a content block, not a top-level event; other
	// user content (e.g. the prompt echo) is not forwarded. The Anthropic
	// Messages API batches ALL results for a set of parallel tool_use calls into
	// ONE user message, so emit one event per tool_result block — returning only
	// the first would silently drop the rest (the common parallel-tools case).
	if raw.Type == "user" && raw.Message != nil {
		var evs []Event
		for _, block := range raw.Message.Content {
			if block.Type == "tool_result" {
				evs = append(evs, Event{
					Kind: EventToolResult,
					ToolResult: &ToolResultEvent{
						ToolUseID: block.ToolUseID,
						Content:   toolResultText(block.Content),
						IsError:   block.IsError,
					},
				})
			}
		}
		return evs
	}

	if raw.Type == "result" {
		// Surface the turn's token usage (tether#48) BEFORE the result. The
		// result is the turn-closer (finalizes the frontend turn); emitting usage
		// first means the frontend still has the open turn bubble to attach it to.
		var evs []Event
		if raw.Usage != nil {
			evs = append(evs, Event{Kind: EventUsage, Usage: &UsageEvent{
				Input:  raw.Usage.InputTokens,
				Output: raw.Usage.OutputTokens,
			}})
		}
		return append(evs, Event{Kind: EventResult, Text: raw.Result})
	}

	if raw.Type == "rate_limit_event" {
		return []Event{{Kind: EventRateLimit}}
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

// toolResultText flattens a tool_result block's `content` to plain text. cc
// sends it either as a JSON string or as an array of content blocks
// ([{type:"text",text:"…"}]) — handle both; anything else yields "" (tether#38).
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var out string
		for _, b := range blocks {
			out += b.Text
		}
		return out
	}
	return ""
}

// Stub providers for future providers (D-17a §5).

// CursorProvider — stub; see D-17a §5.2.
type CursorProvider struct{}

func (CursorProvider) Name() string { return "cursor" }
func (CursorProvider) Spawn(_ context.Context, _ SpawnConfig) (Session, error) {
	return nil, fmt.Errorf("cursor provider: not yet implemented")
}
