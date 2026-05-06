package jsonl

// Class is the three-class taxonomy from spec §5.6 (F-04).
//
//	EVENT  — type=user / type=assistant. Have uuid + parent chain.
//	         Mapped to wire `output.agent-event`, broadcast to all
//	         attached devices via Stream 2 (永不丢).
//	HOOK   — type=attachment AND attachment.hookEvent non-empty.
//	         Mapped to wire `output.hook-event`. Drives the daemon
//	         hook state machine (PreToolUse approval, etc.).
//	STATE  — type=permission-mode / last-prompt / file-history-snapshot
//	         / ai-title / system, OR a type=attachment that has no
//	         hookEvent (rare; observed historically as plain payloads).
//	         Updates session view; NOT broadcast on events stream.
//
// ClassUnknown is reserved for forward-compatibility: cc may add new
// type values in patch versions. Unknown types are routed as STATE
// (defensive; STATE has the lowest blast radius — it neither broadcasts
// nor mutates the hook FSM) and the watcher's metrics counter
// increments so we notice in production.
type Class int

const (
	ClassUnknown Class = iota
	ClassEvent
	ClassHook
	ClassState
)

// String renders Class for log lines.
func (c Class) String() string {
	switch c {
	case ClassEvent:
		return "EVENT"
	case ClassHook:
		return "HOOK"
	case ClassState:
		return "STATE"
	default:
		return "UNKNOWN"
	}
}

// Classify implements the F-04 taxonomy decision. Pure function — no
// side effects, no allocation in the hot path. Total over Record (no
// nil deref: Attachment is pointer-checked).
//
// The doc.go classification table is the authoritative reference; this
// function is its executable form. If you change the semantics here,
// update doc.go in the same commit.
func Classify(rec Record) Class {
	switch rec.Type {
	case RecordTypeUser, RecordTypeAssistant:
		return ClassEvent

	case RecordTypeAttachment:
		// HOOK iff attachment.hookEvent non-empty. Empty hookEvent
		// downgrades to STATE — observed historically as receipt /
		// metadata stubs that don't drive the hook FSM.
		if rec.Attachment != nil && rec.Attachment.HookEvent != "" {
			return ClassHook
		}
		return ClassState

	case RecordTypePermissionMode,
		RecordTypeLastPrompt,
		RecordTypeFileHistorySnapshot,
		RecordTypeAITitle,
		RecordTypeSystem:
		return ClassState

	default:
		// Forward-compat: unknown type → STATE (least-blast-radius).
		// Caller logs / counts; we don't drop.
		return ClassUnknown
	}
}

// RouteAsState returns true when the class should be treated as STATE
// for routing purposes. ClassUnknown rolls up to STATE here — separate
// from Classify() so callers can still distinguish "really STATE" from
// "unknown, treated as STATE for safety" via metrics.
func RouteAsState(c Class) bool {
	return c == ClassState || c == ClassUnknown
}
