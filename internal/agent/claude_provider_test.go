package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestParseLine_StreamEventTextDelta verifies that --include-partial-messages
// stream_event lines yield token-level EventText events.
func TestParseLine_StreamEventTextDelta(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"So"}}}`)
	ev := s.parseLine(line)
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
	ev := s.parseLine(line)
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
	if ev := s.parseLine(line); ev != nil {
		t.Errorf("expected nil for empty thinking_delta, got %+v", ev)
	}
}

// TestParseLine_StreamEventSignatureDelta confirms that thinking-block signature
// deltas are silently dropped — they're not user-visible content.
func TestParseLine_StreamEventSignatureDelta(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","text":""}}}`)
	if ev := s.parseLine(line); ev != nil {
		t.Errorf("expected nil for signature_delta, got %+v", ev)
	}
}

// TestParseLine_AssistantTextSkipped — with partial-messages on, the final
// `assistant` event's text block is redundant. Only tool_use blocks should
// surface from it.
func TestParseLine_AssistantTextSkipped(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"Soft clouds drift above."}]}}`)
	if ev := s.parseLine(line); ev != nil {
		t.Errorf("expected nil for assistant text block (already streamed), got %+v", ev)
	}
}

// TestParseLine_AssistantToolUse — tool_use blocks still surface from
// assistant events; their complete input JSON is more reliable than
// reassembling partial input_json_deltas.
func TestParseLine_AssistantToolUse(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_123","name":"Bash","input":{"command":"ls"}}]}}`)
	ev := s.parseLine(line)
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

// TestParseLine_SystemInit confirms session_id capture still works.
func TestParseLine_SystemInit(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"system","subtype":"init","session_id":"abc-123"}`)
	ev := s.parseLine(line)
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
	ev := s.parseLine(line)
	if ev == nil || ev.Kind != EventResult {
		t.Fatalf("expected EventResult, got %+v", ev)
	}
	if ev.Text != "1, 2, 3" {
		t.Errorf("Text = %q, want 1, 2, 3", ev.Text)
	}
}

// TestParseLine_ControlResponseIgnored — cc's reply to an outbound
// control_request (e.g. the T9 interrupt request written by
// ccSession.Interrupt) must be silently dropped: no Event, and critically
// not a bad/error Event that would surface as noise in the chat stream.
func TestParseLine_ControlResponseIgnored(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"tether-interrupt-1"}}`)
	if ev := s.parseLine(line); ev != nil {
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
	if ev := s.parseLine(line); ev != nil {
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
