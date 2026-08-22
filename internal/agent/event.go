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
//
// # RUN IDENTITY (tether#148) — the provider ↔ registry half of this contract
//
// `RunID` names the RUN that produced an event: one accepted prompt and
// everything the agent said in answer to it. It exists for exactly one consumer
// question, and that question could not be answered before it: when a
// turn-closing event arrives (EventResult / EventError, see isTerminal), is this
// the FIRST end-of-turn signal from its run, or a second one from a run that has
// already been counted down?
//
// Registry.fanOut has to answer it because one run really does produce two
// turn-closers. opencodeSession's run goroutine emits an EventError for a scan
// failure (or a non-zero run exit, or an SSE session.error) and then,
// unconditionally, its terminal EventResult — while Entry.turnsInFlight counted
// ONE delivery. Applying both takes a turn away from whatever delivery landed in
// between, and the session then reports `idle` for the whole of a turn the user is
// waiting on. tether#145 tried to separate the cases from the COUNT alone and
// could only reach one of the two reachable interleavings: a run's duplicate
// signal and a different, refused delivery's own signal present the registry with
// the same count, the same kinds and the same order, and demand opposite answers.
// Which one it is depends on whether the result was emitted before or after the
// next delivery was accepted — knowable only here, at the seam that emits it.
//
// # The contract
//
//   - Run ids are per-SESSION and NON-DECREASING in emission order. Each provider
//     owes that ordering, and neither gets it from the mint alone: cc's readLoop is
//     one goroutine, and opencode mints inside the window its `busy` gate holds,
//     placed so that every emit carrying an id happens before the release (see
//     opencodeSession.runSeq, which states where the mint has to sit). The ordering
//     is what lets the consumer keep a single high-water mark instead of a set that
//     grows for the life of the session — see fanOut's settledRun, which also
//     states what breaking it would cost.
//   - Every event a run produces carries that run's id, turn-closers included.
//     Two turn-closers with the SAME id are therefore one run reporting twice, and
//     the second must not count a turn down.
//   - ZERO means "no run", and a zero-run turn-closer is always applied. Three
//     things say it, and all three mean "this cannot be a run's second signal": a
//     prompt REFUSED before any run existed (opencode's busy rejection), an
//     ACCEPTED delivery that never got a run started (opencode's failed serve
//     relaunch — see there for why an id would be actively wrong), and a provider
//     that does not attribute at all.
//
// # A missed emit site degrades at runtime, and a test is what catches it
//
// The zero value is a legal, meaningful RunID, so a provider that forgets to stamp
// a turn-closer compiles and runs — and silently gets tether#103's behaviour back
// for that path. Making it impossible in the type system would mean no Event could
// be written as a composite literal, which is how all 20 production emit sites (11
// in opencode_provider.go, 9 in claude_provider.go) and well over a hundred test
// fixtures build them. So the guard is a test instead:
// TestEveryEventLiteralCarriesARunID parses internal/agent and fails on any Event
// literal with no RunID key, which is the same trade Entry.sendPrompt's own AST
// guard makes and states. Its companion, TestOpenCodeStampsEveryEventWithARun,
// covers the mutation a presence check cannot see: a site that stamps the WRONG id.
//
// # Non-terminal kinds
//
// Stamped too, because "every event carries its run" is a rule a reader can check
// and "the turn-closers do" is one they have to reconstruct. Nothing consumes it
// on those kinds today.
type Event struct {
	Kind       EventKind
	SessionID  string // populated on EventInit; stable for session lifetime
	RunID      int64  // the run this event belongs to; see the RUN IDENTITY section
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
