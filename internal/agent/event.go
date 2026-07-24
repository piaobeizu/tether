package agent

import "encoding/json"

// EventKind discriminates daemon-internal agent events.
// These are NOT tygo-generated and must NOT appear in wire.gen.ts (D-17a §7, D-22 §2.2).
type EventKind string

const (
	EventInit       EventKind = "init"        // system/init — carries SessionID
	EventText       EventKind = "text"        // assistant text chunk
	EventThinking   EventKind = "thinking"    // extended-thinking delta (tether#34)
	EventToolUse    EventKind = "tool_use"    // tool_use block extracted from assistant content
	EventToolResult EventKind = "tool_result" // tool_result block extracted from a user event (tether#38)
	EventUsage      EventKind = "usage"       // token usage for the turn, from the result event (tether#48)
	EventResult     EventKind = "result"      // turn result / stop reason
	EventRateLimit  EventKind = "rate_limit"  // rate_limit_event
	EventError      EventKind = "error"       // daemon-side error
)

// Event is the daemon-internal representation of a cc output event.
// Translated from stream-json lines by ClaudeCodeProvider.
type Event struct {
	Kind       EventKind
	SessionID  string // populated on EventInit; stable for session lifetime
	Text       string // EventText
	ToolUse    *ToolUseEvent
	ToolResult *ToolResultEvent
	Usage      *UsageEvent // EventUsage
	Err        error       // EventError
}

// isTerminal reports whether k is a turn-closing event that MUST be delivered
// reliably — never dropped under backpressure. Losing one leaves the consumer's
// turn open forever: the frontend clears its "thinking…" indicator on both the
// result and error envelopes (tether#14), and some opencode error paths return
// before emitting any EventResult, so the error is the only turn-closer. All
// other kinds (EventText / EventThinking token deltas, tool_use, usage, init,
// rate_limit) stay best-effort and may be dropped when the buffer is full —
// intentional backpressure for high-frequency output.
func isTerminal(k EventKind) bool {
	return k == EventResult || k == EventError
}

// ToolUseEvent carries a tool_use content block extracted from an assistant event.
// NOTE: tool_use is a CONTENT BLOCK inside assistant.message.content[], NOT a
// top-level stream event (D-05a §3, Risk #4). chat_translate.go extracts these.
type ToolUseEvent struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResultEvent carries a tool_result content block extracted from a `user`
// event — the output of a tool cc ran (tether#38). Matched to its ToolUseEvent
// by ToolUseID. Content is flattened to text (cc sends a string or a
// [{type,text}] array in the tool_result's content field).
type ToolResultEvent struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// UsageEvent carries the turn's token usage, extracted from cc's `result`
// event's top-level `usage` object (tether#48). Only the plain input/output
// token counts are surfaced — cache_creation / cache_read tokens and
// total_cost_usd are deliberately omitted (the badge shows in/out only, and a
// cc-priced cost estimate can mislead a tether user on their own key/quota).
// Emitted just before the turn's EventResult so the frontend can attach it to
// the still-open turn bubble before the result finalizes the turn. Best-effort
// (not terminal): if dropped under backpressure the badge simply doesn't show.
type UsageEvent struct {
	Input  int
	Output int
}
