package agent

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// first returns the first event parseLine produced, or nil — a shim so the
// single-event tests read as before now that parseLine returns []Event.
func first(evs []Event) *Event {
	if len(evs) == 0 {
		return nil
	}
	return &evs[0]
}

// TestParseLine_StreamEventTextDelta verifies that --include-partial-messages
// stream_event lines yield token-level EventText events.
func TestParseLine_StreamEventTextDelta(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"So"}}}`)
	ev := first(s.parseLine(line))
	if ev == nil {
		t.Fatal("expected EventText, got nil")
	}
	if ev.Kind != EventText {
		t.Errorf("Kind = %q, want %q", ev.Kind, EventText)
	}
	if ev.Text != "So" {
		t.Errorf("Text = %q, want %q", ev.Text, "So")
	}
}

// TestParseLine_StreamEventThinkingDelta verifies that extended-thinking
// deltas (delta.type=="thinking_delta", content in delta.thinking) surface as
// EventThinking with the thinking text (tether#34) — they used to be dropped.
func TestParseLine_StreamEventThinkingDelta(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants"}}}`)
	ev := first(s.parseLine(line))
	if ev == nil {
		t.Fatal("expected EventThinking, got nil")
	}
	if ev.Kind != EventThinking {
		t.Errorf("Kind = %q, want %q", ev.Kind, EventThinking)
	}
	if ev.Text != "The user wants" {
		t.Errorf("Text = %q, want %q", ev.Text, "The user wants")
	}
}

// TestParseLine_StreamEventThinkingDeltaEmpty — an empty thinking delta (no
// content) is dropped, mirroring the empty-text_delta guard.
func TestParseLine_StreamEventThinkingDeltaEmpty(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for empty thinking_delta, got %+v", ev)
	}
}

// TestParseLine_StreamEventSignatureDelta confirms that thinking-block signature
// deltas are silently dropped — they're not user-visible content.
func TestParseLine_StreamEventSignatureDelta(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","text":""}}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for signature_delta, got %+v", ev)
	}
}

// TestParseLine_AssistantTextSkipped — with partial-messages on, the final
// `assistant` event's text block is redundant. Only tool_use blocks should
// surface from it.
func TestParseLine_AssistantTextSkipped(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Soft clouds drift above."}]}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for assistant text block (already streamed), got %+v", ev)
	}
}

// TestParseLine_AssistantToolUse — tool_use blocks still surface from
// assistant events; their complete input JSON is more reliable than
// reassembling partial input_json_deltas.
func TestParseLine_AssistantToolUse(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Bash","input":{"command":"ls"}}]}}`)
	ev := first(s.parseLine(line))
	if ev == nil {
		t.Fatal("expected EventToolUse, got nil")
	}
	if ev.Kind != EventToolUse {
		t.Errorf("Kind = %q, want %q", ev.Kind, EventToolUse)
	}
	if ev.ToolUse == nil {
		t.Fatal("ToolUse nil")
	}
	if ev.ToolUse.ID != "toolu_123" || ev.ToolUse.Name != "Bash" {
		t.Errorf("ToolUse = %+v, want id=toolu_123 name=Bash", ev.ToolUse)
	}
	if string(ev.ToolUse.Input) != `{"command":"ls"}` {
		t.Errorf("Input = %s, want {\"command\":\"ls\"}", ev.ToolUse.Input)
	}
}

// TestParseLine_UserToolResult — tool_result blocks on a `user` event surface as
// EventToolResult with tool_use_id, flattened content, and is_error (tether#38).
func TestParseLine_UserToolResult(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"file contents","is_error":false}]}}`)
	ev := first(s.parseLine(line))
	if ev == nil {
		t.Fatal("expected EventToolResult, got nil")
	}
	if ev.Kind != EventToolResult {
		t.Errorf("Kind = %q, want %q", ev.Kind, EventToolResult)
	}
	if ev.ToolResult == nil {
		t.Fatal("ToolResult nil")
	}
	if ev.ToolResult.ToolUseID != "toolu_123" {
		t.Errorf("ToolUseID = %q, want toolu_123", ev.ToolResult.ToolUseID)
	}
	if ev.ToolResult.Content != "file contents" {
		t.Errorf("Content = %q, want %q", ev.ToolResult.Content, "file contents")
	}
	if ev.ToolResult.IsError {
		t.Error("IsError = true, want false")
	}
}

// TestParseLine_UserToolResult_ArrayContent — cc may send the tool_result content
// as an array of blocks ([{type:text,text}]); toolResultText flattens to text,
// and is_error is carried through.
func TestParseLine_UserToolResult_ArrayContent(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":[{"type":"text","text":"line1\n"},{"type":"text","text":"line2"}],"is_error":true}]}}`)
	ev := first(s.parseLine(line))
	if ev == nil || ev.Kind != EventToolResult || ev.ToolResult == nil {
		t.Fatalf("expected EventToolResult, got %+v", ev)
	}
	if ev.ToolResult.Content != "line1\nline2" {
		t.Errorf("Content = %q, want %q", ev.ToolResult.Content, "line1\nline2")
	}
	if !ev.ToolResult.IsError {
		t.Error("IsError = false, want true")
	}
}

// TestParseLine_UserToolResultBatch — parallel tool calls come back as MULTIPLE
// tool_result blocks in ONE user message (the Anthropic API batches them);
// parseLine must emit an event for EACH, not just the first (tether#38 review
// MAJOR: returning only the first silently dropped parallel-tool results).
func TestParseLine_UserToolResultBatch(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"r1"},{"type":"tool_result","tool_use_id":"tu2","content":"r2","is_error":true}]}}`)
	evs := s.parseLine(line)
	if len(evs) != 2 {
		t.Fatalf("expected 2 EventToolResult, got %d", len(evs))
	}
	if evs[0].ToolResult == nil || evs[0].ToolResult.ToolUseID != "tu1" || evs[0].ToolResult.Content != "r1" {
		t.Errorf("evs[0].ToolResult = %+v, want tu1/r1", evs[0].ToolResult)
	}
	if evs[1].ToolResult == nil || evs[1].ToolResult.ToolUseID != "tu2" || !evs[1].ToolResult.IsError {
		t.Errorf("evs[1].ToolResult = %+v, want tu2/is_error", evs[1].ToolResult)
	}
}

// TestParseLine_UserPromptEchoSkipped — a user event that is just the prompt
// echo (text block, no tool_result) must not be forwarded.
func TestParseLine_UserPromptEchoSkipped(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for user prompt echo, got %+v", ev)
	}
}

// TestToolResultText covers the string | array | empty | non-text cases.
func TestToolResultText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"string", `"hello"`, "hello"},
		{"array", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "ab"},
		{"empty", ``, ""},
		{"number", `42`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolResultText(json.RawMessage(c.raw)); got != c.want {
				t.Errorf("toolResultText(%s) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestParseLine_SystemInit confirms session_id capture still works.
func TestParseLine_SystemInit(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"system","subtype":"init","session_id":"abc-123"}`)
	ev := first(s.parseLine(line))
	if ev == nil || ev.Kind != EventInit {
		t.Fatalf("expected EventInit, got %+v", ev)
	}
	if s.sid != "abc-123" {
		t.Errorf("sid = %q, want abc-123", s.sid)
	}
}

// TestParseLine_Result confirms turn-completion still fires EventResult.
func TestParseLine_Result(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"result","subtype":"success","result":"1, 2, 3"}`)
	ev := first(s.parseLine(line))
	if ev == nil || ev.Kind != EventResult {
		t.Fatalf("expected EventResult, got %+v", ev)
	}
	if ev.Text != "1, 2, 3" {
		t.Errorf("Text = %q, want 1, 2, 3", ev.Text)
	}
}

// TestParseLine_ResultWithUsage — a result event carrying a top-level `usage`
// object yields the turn's token usage as an EventUsage emitted BEFORE the
// EventResult (tether#48). The frontend needs usage to arrive while the turn
// bubble is still open, so ordering matters: usage first, result (turn-closer)
// second. Only input/output are surfaced; cache_* and total_cost_usd are
// ignored per scope.
func TestParseLine_ResultWithUsage(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"result","subtype":"success","result":"done","total_cost_usd":0.0123,"usage":{"input_tokens":1234,"output_tokens":856,"cache_creation_input_tokens":40,"cache_read_input_tokens":9000}}`)
	evs := s.parseLine(line)
	if len(evs) != 2 {
		t.Fatalf("expected [EventUsage, EventResult], got %d events: %+v", len(evs), evs)
	}
	if evs[0].Kind != EventUsage {
		t.Errorf("evs[0].Kind = %q, want %q (usage must precede result)", evs[0].Kind, EventUsage)
	}
	if evs[0].Usage == nil || evs[0].Usage.Input != 1234 || evs[0].Usage.Output != 856 {
		t.Errorf("evs[0].Usage = %+v, want {Input:1234 Output:856}", evs[0].Usage)
	}
	if evs[1].Kind != EventResult || evs[1].Text != "done" {
		t.Errorf("evs[1] = {%q, %q}, want {result, done}", evs[1].Kind, evs[1].Text)
	}
}

// TestParseLine_ResultNoUsage — a result event with no `usage` object emits
// ONLY the EventResult (no empty/zero EventUsage), so turns where cc omits
// usage don't produce a bogus 0↑/0↓ badge.
func TestParseLine_ResultNoUsage(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"result","subtype":"success","result":"done"}`)
	evs := s.parseLine(line)
	if len(evs) != 1 || evs[0].Kind != EventResult {
		t.Fatalf("expected exactly [EventResult], got %+v", evs)
	}
}

// TestSessionID_UnblocksOnDeath — a cc that exits before emitting system/init
// (e.g. a failed `--resume`, tether#49) must NOT park SessionID() forever:
// readLoop closing `done` unblocks it, returning "" so serveChat surfaces an
// error / spawns fresh instead of hanging the turn in "thinking…".
//
// This pins the channel mechanics in isolation (no subprocess). The same case
// against a real subprocess reproducing cc's measured failure output is
// TestSpawn_ResumeUnknownSessionDiesBeforeInit (tether#53).
func TestSessionID_UnblocksOnDeath(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{}), done: make(chan struct{})}
	close(s.done) // readLoop returned (process exited) before any system/init
	got := make(chan string, 1)
	go func() { got <- s.SessionID() }()
	select {
	case sid := <-got:
		if sid != "" {
			t.Errorf("SessionID() = %q, want \"\" on death-before-init", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SessionID() blocked forever after done closed (tether#49 wedge)")
	}
}

// TestSessionID_ResolvesOnInit — the normal path: system/init closes sidReady
// with the sid set, so SessionID() returns the real sid (and death afterwards
// is irrelevant).
func TestSessionID_ResolvesOnInit(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{}), done: make(chan struct{})}
	s.sid = "sess-xyz"
	close(s.sidReady)
	if got := s.SessionID(); got != "sess-xyz" {
		t.Errorf("SessionID() = %q, want sess-xyz", got)
	}
}

// TestParseLine_ControlResponseIgnored — cc's reply to an outbound
// control_request (e.g. the T9 interrupt request written by
// ccSession.Interrupt) must be silently dropped: no Event, and critically
// not a bad/error Event that would surface as noise in the chat stream.
func TestParseLine_ControlResponseIgnored(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"tether-interrupt-1"}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for control_response, got %+v", ev)
	}
}

// TestParseLine_ControlResponseErrorIgnored — an error-subtype
// control_response (e.g. cc rejecting an interrupt request it couldn't
// honor) must also be dropped quietly, not surfaced as EventError; T9
// doesn't correlate/await control_request replies at all.
func TestParseLine_ControlResponseErrorIgnored(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"control_response","response":{"subtype":"error","request_id":"tether-interrupt-1","error":"no active turn"}}`)
	if ev := first(s.parseLine(line)); ev != nil {
		t.Errorf("expected nil for control_response error, got %+v", ev)
	}
}

// ─── Interrupt (tether#8 T9) ────────────────────────────────────────────────
//
// Interrupt used to send SIGINT to the cc subprocess. tether holds cc's
// stdin open across the whole session (--input-format stream-json), so
// killing/signaling the process would defeat resumability; these tests pin
// the replacement behavior: a stream-json control_request written through
// the same mu-guarded encoder SendPrompt uses, and NO process signaling.

// TestInterrupt_WritesControlRequest asserts the emitted JSON shape:
// {"type":"control_request","request_id":"<non-empty>","request":{"subtype":"interrupt"}}.
func TestInterrupt_WritesControlRequest(t *testing.T) {
	var buf bytes.Buffer
	s := &ccSession{enc: json.NewEncoder(&buf), sidReady: make(chan struct{})}

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("Interrupt did not write valid JSON: %v (%q)", err, buf.String())
	}
	if got["type"] != "control_request" {
		t.Errorf(`type = %v, want "control_request"`, got["type"])
	}
	reqID, _ := got["request_id"].(string)
	if reqID == "" {
		t.Error("request_id is empty, want a unique non-empty id")
	}
	req, ok := got["request"].(map[string]any)
	if !ok {
		t.Fatalf("request field missing or not an object: %+v", got)
	}
	if req["subtype"] != "interrupt" {
		t.Errorf(`request.subtype = %v, want "interrupt"`, req["subtype"])
	}
}

// TestInterrupt_UniqueRequestIDsPerCall ensures repeated Interrupt() calls
// (e.g. rapid double-clicks on the pause button) don't reuse a request_id.
func TestInterrupt_UniqueRequestIDsPerCall(t *testing.T) {
	var buf bytes.Buffer
	s := &ccSession{enc: json.NewEncoder(&buf), sidReady: make(chan struct{})}

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt #1: %v", err)
	}
	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt #2: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 written lines, got %d: %q", len(lines), buf.String())
	}
	var a, b map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &a); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &b); err != nil {
		t.Fatalf("line 2 not valid JSON: %v", err)
	}
	if a["request_id"] == b["request_id"] {
		t.Errorf("expected distinct request_id per call, got %q both times", a["request_id"])
	}
}

// TestInterrupt_DoesNotSignalProcess pins the core T9 behavior change: the
// old implementation dereferenced s.cmd.Process to send SIGINT — with s.cmd
// left as its zero value (nil *exec.Cmd), that would panic. Interrupt must
// never touch s.cmd at all now; it only writes to the stdin encoder.
func TestInterrupt_DoesNotSignalProcess(t *testing.T) {
	var buf bytes.Buffer
	s := &ccSession{enc: json.NewEncoder(&buf), sidReady: make(chan struct{})}

	if err := s.Interrupt(); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if s.cmd != nil {
		t.Errorf("s.cmd = %+v, want nil — Interrupt must not touch/signal the process", s.cmd)
	}
}

// ─── Spawn cwd (tether#51) ──────────────────────────────────────────────────
//
// cc's on-disk conversation and file edits are cwd-scoped, so the spawned
// subprocess MUST run in the workspace directory rather than inheriting the
// daemon's own startup cwd. These tests spawn a real subprocess — the fake cc
// from fakecc_test.go — and assert its actual working directory, hermetically:
// no real `claude` binary and (since tether#53 replaced the old `#!/bin/sh`
// stand-in) no system shell required either.

// drainEvents reads sess.Events() until it closes (the fake subprocess has
// exited), bounded so a bug that hangs Spawn/readLoop fails the test instead
// of the suite.
func drainEvents(t *testing.T, sess Session) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sess.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for fake cc subprocess to exit")
		}
	}
}

// collectUntilResult reads events until the turn closes (EventResult) or the
// channel does, bounded so a hang fails this test rather than the suite.
func collectUntilResult(t *testing.T, sess Session) []Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var evs []Event
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
			if ev.Kind == EventResult {
				return evs
			}
		case <-deadline:
			t.Fatalf("timed out after %d events, never saw EventResult: %v", len(evs), eventKinds(evs))
		}
	}
}

// collectUntilClosed reads every event until Events() closes.
func collectUntilClosed(t *testing.T, sess Session) []Event {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var evs []Event
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for Events() to close; got %v", eventKinds(evs))
		}
	}
}

func eventKinds(evs []Event) []EventKind {
	kinds := make([]EventKind, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	return kinds
}

// closeOnce wraps sess.Close so a test can close at a CHOSEN point — necessary
// whenever it inspects the subprocess's exit status, or reads a writer that
// os/exec only flushes when cmd.Wait() returns (the withStderr sink) — while
// still deferring a safety net for the early-t.Fatalf path. Calling Close twice
// would call cmd.Wait twice and report "Wait was already called"; the sync.Once
// makes the second call a no-op that replays the first result.
func closeOnce(sess Session) func() error {
	var (
		once sync.Once
		err  error
	)
	return func() error {
		once.Do(func() { err = sess.Close() })
		return err
	}
}

// closeExpectingCleanExit closes sess and fails the test unless the subprocess
// exited 0. Worth asserting rather than discarding: if the fake dies badly — a
// panic, or `go test -race` finding a data race inside it (exit 66) — cmd.Wait's
// error is the ONLY signal, so a test that throws it away stays green while its
// subprocess crashed.
func closeExpectingCleanExit(t *testing.T, sess Session) {
	t.Helper()
	if err := sess.Close(); err != nil {
		t.Errorf("Close() = %v, want nil — the fake cc subprocess must exit 0", err)
	}
}

// resolvedEqual compares two paths after resolving symlinks on both sides —
// t.TempDir() on some platforms (and /tmp generally) can be a symlink, so a
// literal string compare would spuriously fail even when the subprocess ran
// in the right place.
func resolvedEqual(t *testing.T, got, want string) {
	t.Helper()
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", got, err)
	}
	wantResolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", want, err)
	}
	if gotResolved != wantResolved {
		t.Errorf("subprocess cwd = %q, want %q", gotResolved, wantResolved)
	}
}

// TestSpawn_SetsCmdDir asserts that a non-empty SpawnConfig.Workdir becomes
// the spawned subprocess's actual working directory (tether#51) — before
// this fix, cmd.Dir was never set at all, so cc always inherited the
// daemon's own cwd regardless of the requested workspace.
func TestSpawn_SetsCmdDir(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true // record the invocation and exit; no turn needed here
	workdir := t.TempDir()

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		Workdir: workdir,
		Env:     h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)

	drainEvents(t, sess)
	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	resolvedEqual(t, recs[0].Cwd, workdir)
}

// TestSpawn_EmptyWorkdirFallsBackToProcessCwd asserts that an empty
// SpawnConfig.Workdir keeps today's behavior — the subprocess inherits the
// daemon's own cwd (os.Getwd()) — rather than running in some empty/root
// directory implied by exec's zero-value Dir.
//
// This one is a SEMANTICS PIN, not a regression guard: it passes both with and
// without cmd.Dir set, because setting Dir to the process cwd is observationally
// identical to leaving it empty. Its job is to fail if someone later changes the
// fallback (e.g. to "" meaning / or to a hardcoded default), which would silently
// relocate every agent spawned by an embedder that doesn't set Workdir.
// TestSpawn_SetsCmdDir is the actual regression guard.
func TestSpawn_EmptyWorkdirFallsBackToProcessCwd(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{Env: h.Env()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)

	drainEvents(t, sess)

	wantWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	resolvedEqual(t, recs[0].Cwd, wantWd)
}

// ─── Spawn against the fake cc: full-turn + failure paths (tether#53) ───────
//
// Until now the only end-to-end test of the argv → subprocess → stream-json →
// Event chain was TestClaudeStreaming_E2E, which needs a real `claude` binary
// and a live API key and is therefore behind `-tags=integration`. Everything
// below runs by default against the fake cc, so the chain has a deterministic
// guard; the real-cc test stays as the fidelity backstop.

// TestSpawn_StreamsFullTurn is the happy path: one prompt in, and the exact
// event sequence tether depends on comes out — EventInit carrying the session
// id, several EventText deltas that reassemble into the reply, EventUsage
// immediately BEFORE the turn-closing EventResult.
//
// The sharpest thing it proves is something no unit test could: the fake emits
// system/hook_started and system/hook_response BEFORE system/init (the measured
// order), and parseLine still lands on the right events — i.e. nothing in the
// chain assumes init is the first line it sees.
func TestSpawn_StreamsFullTurn(t *testing.T) {
	h := newFakeCCHarness(t)
	h.Reply = "alpha beta gamma delta epsilon"
	workdir := t.TempDir()
	const prompt = "hello there"

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{Workdir: workdir, Env: h.Env()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)

	if err := sess.SendPrompt(context.Background(), prompt); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	evs := collectUntilResult(t, sess)

	// One EventText per text_delta the fake streams, derived rather than
	// hardcoded so a change to the fake's chunking is not a spurious failure
	// here — the ORDER is what this test is pinning.
	wantKinds := []EventKind{EventInit}
	for range fakeCCChunks(h.Reply) {
		wantKinds = append(wantKinds, EventText)
	}
	wantKinds = append(wantKinds, EventUsage, EventResult)
	if got := eventKinds(evs); !reflect.DeepEqual(got, wantKinds) {
		t.Fatalf("event kinds = %v, want %v", got, wantKinds)
	}

	sid := sess.SessionID()
	if sid == "" {
		t.Fatal("SessionID() is empty after a successful turn")
	}
	if evs[0].SessionID != sid {
		t.Errorf("EventInit SessionID = %q, want %q", evs[0].SessionID, sid)
	}

	var assembled string
	for _, ev := range evs {
		if ev.Kind == EventText {
			assembled += ev.Text
		}
	}
	if assembled != h.Reply {
		t.Errorf("assembled text = %q, want %q", assembled, h.Reply)
	}

	usage := evs[len(evs)-2]
	if usage.Usage == nil || usage.Usage.Input != len(prompt) || usage.Usage.Output != len(h.Reply) {
		t.Errorf("usage = %+v, want {Input:%d Output:%d}", usage.Usage, len(prompt), len(h.Reply))
	}
	if result := evs[len(evs)-1]; result.Text != h.Reply {
		t.Errorf("EventResult Text = %q, want %q", result.Text, h.Reply)
	}

	// The subprocess really did run where we asked, with the sid it reported.
	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	if recs[0].SessionID != sid {
		t.Errorf("fake cc used sid %q, provider reported %q", recs[0].SessionID, sid)
	}
	resolvedEqual(t, recs[0].Cwd, workdir)
}

// TestSpawn_ResumeUnknownSessionDiesBeforeInit is tether#49's die-before-init
// case, moved off hand-closed channels and onto a real subprocess that
// reproduces cc's measured failure shape (mem_2ruSlrHR ③).
//
// TestSessionID_UnblocksOnDeath pins the channel mechanics in isolation; this
// pins the whole chain that mechanism exists for: a `--resume` of a session this
// cwd does not own makes cc exit 1 having emitted NO system/init, so
// SessionID() must return "" (unblocked by readLoop closing `done`) rather than
// parking the caller forever — the wedge that left a turn stuck in "thinking…".
//
// It also pins two facts tether#50's fallback path will build on: the single
// `result` line has result:null, so parseLine yields an EventResult with EMPTY
// text (the blank bubble #50 must swallow); and cc's diagnosis is on stderr, not
// in the event stream — observable here only through the withStderr seam.
func TestSpawn_ResumeUnknownSessionDiesBeforeInit(t *testing.T) {
	h := newFakeCCHarness(t) // no session seeded: nothing is resumable
	var stderr syncBuffer

	p := NewClaudeCodeProvider(fakeCCPath(t), withStderr(&stderr))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		ResumeSessionID: fakeCCTestUUID,
		Workdir:         t.TempDir(),
		Env:             h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	closeSess := closeOnce(sess)
	defer func() { _ = closeSess() }() // safety net if an assertion below fatals

	evs := collectUntilClosed(t, sess)

	if sid := sess.SessionID(); sid != "" {
		t.Errorf("SessionID() = %q, want \"\" — cc never emitted system/init", sid)
	}
	if got := eventKinds(evs); !reflect.DeepEqual(got, []EventKind{EventResult}) {
		t.Fatalf("event kinds = %v, want exactly [result] (no init, no usage)", got)
	}
	if evs[0].Text != "" {
		t.Errorf("EventResult Text = %q, want \"\" (cc sent result:null)", evs[0].Text)
	}

	// Close is what calls cmd.Wait(), which both yields the exit status and
	// finishes os/exec's stderr-copying goroutine — so the sink is only safe to
	// read after this point.
	code, ok := exitCodeOf(closeSess())
	if !ok || code != 1 {
		t.Errorf("Close() exit status = %d (recognised=%v), want 1", code, ok)
	}
	if want := fakeCCNoConversation + fakeCCTestUUID; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	if recs[0].ResumeFlag != fakeCCTestUUID {
		t.Errorf("fake cc saw --resume %q, want %q", recs[0].ResumeFlag, fakeCCTestUUID)
	}
	if !recs[0].ResumeFailed {
		t.Error("fake cc did not take the resume-failure path")
	}
}

// TestSpawn_ResumeKnownSessionSucceeds — the other half of the resume contract
// through the provider: when the session IS resumable from this cwd, cc runs
// normally and the sid does not drift (mem_2ruSlrHR ②). Together with the test
// above, this is what makes a try-resume-then-fall-back implementation
// (tether#50) testable at all: both branches are now reachable on demand.
func TestSpawn_ResumeKnownSessionSucceeds(t *testing.T) {
	h := newFakeCCHarness(t)
	workdir := t.TempDir()
	h.SeedSession(t, fakeCCTestUUID, workdir)

	var stderr syncBuffer
	p := NewClaudeCodeProvider(fakeCCPath(t), withStderr(&stderr))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		ResumeSessionID: fakeCCTestUUID,
		Workdir:         workdir,
		Env:             h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	closeSess := closeOnce(sess)
	defer func() { _ = closeSess() }() // safety net if an assertion below fatals

	if err := sess.SendPrompt(context.Background(), "again"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	evs := collectUntilResult(t, sess)

	if sid := sess.SessionID(); sid != fakeCCTestUUID {
		t.Errorf("SessionID() = %q, want the resumed %q (sid must not drift)", sid, fakeCCTestUUID)
	}
	if evs[0].Kind != EventInit || evs[0].SessionID != fakeCCTestUUID {
		t.Errorf("first event = %+v, want EventInit for %q", evs[0], fakeCCTestUUID)
	}
	if last := evs[len(evs)-1]; last.Kind != EventResult || last.Text == "" {
		t.Errorf("last event = %+v, want a non-empty EventResult", last)
	}

	// Close BEFORE reading the stderr sink, and not from a defer: os/exec copies
	// a non-*os.File stderr on a goroutine that only finishes inside cmd.Wait(),
	// which Close is what calls. Asserting the sink while Close was still
	// deferred made this check unfalsifiable — a mutation that wrote to stderr on
	// a SUCCESSFUL resume passed it 30/30 (tether#53 review MAJOR).
	if err := closeSess(); err != nil {
		t.Errorf("Close() = %v, want nil — a successful resume must exit 0", err)
	}
	if s := stderr.String(); s != "" {
		t.Errorf("stderr = %q, want empty on a successful resume", s)
	}
}

// TestSpawn_ArgvContract pins the command line Spawn builds. It was previously
// untested end to end — the flags only existed as string literals — even though
// each one is load-bearing: --output-format/--input-format stream-json is the
// whole protocol, --include-partial-messages is what makes text arrive as
// deltas instead of one block, and --permission-mode default is what forces
// PreToolUse hooks to fire regardless of the user's settings.json.
//
// It also pins the tether#50 argv RULE, which is the one thing about this
// command line that is not free to drift: a fresh spawn pins the daemon's minted
// id with `--session-id`, a reconnect passes `--resume` alone, and the two flags
// NEVER appear together (mem_2ruSlrHR ⑧ — real cc exits 1 on that combination).
// See TestSpawn_ReconnectPassesResumeAlone and
// TestSpawn_SessionIDWithResumeRejected for the other two thirds of the rule.
//
// CHANGED BY tether#50, on purpose: this test previously asserted the OPPOSITE
// of the last check below — "--session-id %q passed; tether does not mint session
// ids yet". tether#53 wrote that assertion as a deliberate tripwire so that the
// slice which started minting ids could not do so silently. This is that slice.
func TestSpawn_ArgvContract(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		SessionID: fakeCCTestUUID,
		Workdir:   t.TempDir(),
		Env:       h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)
	drainEvents(t, sess)

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	want := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-mode", "default",
		"--session-id", fakeCCTestUUID,
	}
	if !reflect.DeepEqual(recs[0].Argv, want) {
		t.Errorf("argv =\n  %v\nwant\n  %v", recs[0].Argv, want)
	}
	if recs[0].ResumeFlag != "" {
		t.Errorf("--resume %q passed on a FRESH spawn; the two flags are mutually exclusive", recs[0].ResumeFlag)
	}
	if recs[0].SessionIDFlag != fakeCCTestUUID {
		t.Errorf("--session-id = %q, want the minted %q; a fresh session must pin its id so it is resumable later (tether#50)",
			recs[0].SessionIDFlag, fakeCCTestUUID)
	}
}

// TestSpawn_ArgvOmitsSessionIDWhenUnset — an unpinned fresh spawn passes NEITHER
// flag. The daemon always mints (Registry.spawnEntry), so this is the guard that
// the provider stays a faithful courier rather than inventing an id of its own:
// a provider that silently minted would make "was this session pinned?"
// unanswerable from the SpawnConfig alone.
func TestSpawn_ArgvOmitsSessionIDWhenUnset(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{Workdir: t.TempDir(), Env: h.Env()})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)
	drainEvents(t, sess)

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	for _, arg := range recs[0].Argv {
		if arg == "--session-id" || arg == "--resume" {
			t.Errorf("argv contains %s for a SpawnConfig that set neither id: %v", arg, recs[0].Argv)
		}
	}
}

// TestSpawn_ReconnectPassesResumeAlone — the reconnect half of the argv rule: a
// config carrying ONLY ResumeSessionID yields `--resume <sid>` and no
// `--session-id`. Re-pinning on reconnect is both forbidden (⑧) and pointless
// (② — a resumed session's id does not drift), and the combination exits 1, so
// "resume alone" has to be asserted, not assumed.
func TestSpawn_ReconnectPassesResumeAlone(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		ResumeSessionID: fakeCCTestUUID,
		Workdir:         t.TempDir(),
		Env:             h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)
	drainEvents(t, sess)

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	if recs[0].ResumeFlag != fakeCCTestUUID {
		t.Errorf("--resume = %q, want %q", recs[0].ResumeFlag, fakeCCTestUUID)
	}
	if recs[0].SessionIDFlag != "" {
		t.Errorf("--session-id = %q passed alongside --resume; real cc exits 1 on that combination (mem_2ruSlrHR ⑧)",
			recs[0].SessionIDFlag)
	}
}

// TestSpawn_SessionIDWithResumeRejected — setting BOTH ids is refused by Spawn
// itself, before any process starts.
//
// Why this is worth a hard failure rather than a "prefer one" fallback: real cc
// exits 1 on that argv WITHOUT emitting system/init, which is byte-for-byte how a
// failed --resume looks. The Attachment fallback would therefore "handle" the bug
// by quietly starting a fresh session, and the only symptom anyone would ever see
// is context being lost at random. Failing here names the actual cause.
func TestSpawn_SessionIDWithResumeRejected(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		SessionID:       fakeCCTestUUID,
		ResumeSessionID: "99999999-8888-4777-8666-555555555555",
		Workdir:         t.TempDir(),
		Env:             h.Env(),
	})
	if err == nil {
		_ = sess.Close()
		t.Fatal("Spawn accepted both SessionID and ResumeSessionID; want an error")
	}
	if sess != nil {
		t.Errorf("Spawn returned a non-nil Session alongside its error: %#v", sess)
	}
	for _, want := range []string{"mutually exclusive", fakeCCTestUUID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// No process may have been started: the guard runs before exec.
	if _, statErr := os.Stat(h.RecordPath); statErr == nil {
		t.Errorf("fake cc was invoked despite the rejected config: %v", h.Records(t))
	}
}

// TestNewSessionID_ShapeAndUniqueness — the minted id must be a syntactically
// valid v4 uuid (cc validates the --session-id it is handed) and must not repeat,
// since a collision would point two sessions at one transcript.
func TestNewSessionID_ShapeAndUniqueness(t *testing.T) {
	seen := make(map[string]bool, 256)
	for i := 0; i < 256; i++ {
		id := NewSessionID()
		if len(id) != 36 {
			t.Fatalf("NewSessionID() = %q, want 36 chars", id)
		}
		parts := strings.Split(id, "-")
		wantLens := []int{8, 4, 4, 4, 12}
		if len(parts) != len(wantLens) {
			t.Fatalf("NewSessionID() = %q, want 5 dash-separated groups", id)
		}
		for j, p := range parts {
			if len(p) != wantLens[j] {
				t.Fatalf("NewSessionID() = %q: group %d is %d chars, want %d", id, j, len(p), wantLens[j])
			}
			if _, err := hex.DecodeString(p); err != nil {
				t.Fatalf("NewSessionID() = %q: group %d is not hex: %v", id, j, err)
			}
		}
		if parts[2][0] != '4' {
			t.Errorf("NewSessionID() = %q: version nibble is %q, want '4'", id, parts[2][0])
		}
		if v := parts[3][0]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("NewSessionID() = %q: variant nibble is %q, want one of 89ab", id, v)
		}
		if seen[id] {
			t.Fatalf("NewSessionID() repeated %q within 256 calls", id)
		}
		seen[id] = true
	}
}

// TestSpawn_ResumeSessionIDBecomesResumeFlag — a non-empty
// SpawnConfig.ResumeSessionID must reach cc as `--resume <sid>`.
//
// Written under tether#49 as a plumbing guard for a field nothing set yet; since
// tether#50 the registry DOES set it (Registry.Attach, on a reconnect whose sid
// is no longer live), so this now covers a live path. Kept alongside
// TestSpawn_ReconnectPassesResumeAlone, which additionally pins the absence of
// --session-id, because this one is the narrow flag-plumbing assertion and that
// one is the argv-rule assertion; they fail for different reasons.
func TestSpawn_ResumeSessionIDBecomesResumeFlag(t *testing.T) {
	h := newFakeCCHarness(t)
	h.ExitEarly = true

	p := NewClaudeCodeProvider(fakeCCPath(t))
	sess, err := p.Spawn(context.Background(), SpawnConfig{
		ResumeSessionID: fakeCCTestUUID,
		Workdir:         t.TempDir(),
		Env:             h.Env(),
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer closeExpectingCleanExit(t, sess)
	drainEvents(t, sess)

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("fake cc recorded %d invocations, want 1", len(recs))
	}
	if recs[0].ResumeFlag != fakeCCTestUUID {
		t.Errorf("--resume = %q, want %q; full argv %v", recs[0].ResumeFlag, fakeCCTestUUID, recs[0].Argv)
	}
}

// TestSpawn_StderrDefaultsToOsStderr — the withStderr seam must not change what
// production does. A provider built the way the daemon builds it (no options)
// resolves its sink to os.Stderr, exactly as the hardcoded assignment did before
// the seam existed; so does a zero-value literal, so a future direct
// construction can't silently redirect cc's diagnostics to /dev/null.
func TestSpawn_StderrDefaultsToOsStderr(t *testing.T) {
	if got := NewClaudeCodeProvider("/nonexistent").stderrSink(); got != os.Stderr {
		t.Errorf("NewClaudeCodeProvider(...).stderrSink() = %#v, want os.Stderr", got)
	}
	if got := (&ClaudeCodeProvider{ccPath: "/nonexistent"}).stderrSink(); got != os.Stderr {
		t.Errorf("zero-value provider stderrSink() = %#v, want os.Stderr", got)
	}
	var sink syncBuffer
	if got := NewClaudeCodeProvider("/nonexistent", withStderr(&sink)).stderrSink(); got != &sink {
		t.Errorf("withStderr sink = %#v, want the injected writer", got)
	}
}
