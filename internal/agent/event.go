package agent

import "encoding/json"

// EventKind discriminates daemon-internal agent events.
// These are NOT tygo-generated and must NOT appear in wire.gen.ts (D-17a §7, D-22 §2.2).
type EventKind string

const (
	EventInit       EventKind = "init"       // system/init — carries SessionID
	EventText       EventKind = "text"       // assistant text chunk
	EventToolUse    EventKind = "tool_use"   // tool_use block extracted from assistant content
	EventResult     EventKind = "result"     // turn result / stop reason
	EventRateLimit  EventKind = "rate_limit" // rate_limit_event
	EventError      EventKind = "error"      // daemon-side error
)

// Event is the daemon-internal representation of a cc output event.
// Translated from stream-json lines by ClaudeCodeProvider.
type Event struct {
	Kind      EventKind
	SessionID string // populated on EventInit; stable for session lifetime
	Text      string // EventText
	ToolUse   *ToolUseEvent
	Err       error // EventError
}

// ToolUseEvent carries a tool_use content block extracted from an assistant event.
// NOTE: tool_use is a CONTENT BLOCK inside assistant.message.content[], NOT a
// top-level stream event (D-05a §3, Risk #4). chat_translate.go extracts these.
type ToolUseEvent struct {
	ID    string
	Name  string
	Input json.RawMessage
}
