package agent

import (
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
