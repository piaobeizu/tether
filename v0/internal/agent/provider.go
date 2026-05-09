// Package agent defines the AgentProvider seam (spec §5.0 / D-17): a
// Go interface that isolates cc-specific logic from the daemon orchestration
// layer. v0.1 has exactly one implementation, ClaudeCodeProvider, defined
// in this package's claude_provider.go. The seam exists so v0.2+ can add
// codex / opencode without rewriting daemon, but the interface IS expected
// to churn when the second provider lands — see spec §5.0 closing note.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// AgentProvider is the unified abstraction over a backing agent CLI. v0.1
// has one impl (ClaudeCodeProvider). The daemon code paths address the
// agent only through this interface — they never import internal/backend/*
// directly.
type AgentProvider interface {
	// ProviderType returns a stable identifier for the underlying agent.
	// v0.1 returns "claude-code" only; the value lands in wire envelope
	// `output.agent-event.providerType` for client-side disambiguation
	// when v0.2+ adds peers.
	ProviderType() string

	// Spawn starts a fresh agent session in the given working directory.
	// Implementations may require an initial user message (cc does — see
	// claude.Session.Start) and surface that requirement via SpawnOpts.
	Spawn(ctx context.Context, opts SpawnOpts) (*AgentSession, error)

	// Resume re-attaches to an existing agent session by its ID. Providers
	// must rehydrate any provider-side state (cwd, mode, etc.) from their
	// own session bucket.
	Resume(ctx context.Context, sessionID string) (*AgentSession, error)

	// Kill terminates the underlying agent subprocess gracefully (SIGTERM,
	// not SIGINT — per spec F-07 SIGINT puts cc into a zombie state).
	Kill(session *AgentSession) error

	// SendUserMessage queues a user-role text turn for the agent. Returns
	// when the message is handed off (typically synchronously written to
	// stdin); does NOT wait for assistant reply.
	SendUserMessage(session *AgentSession, text string) error

	// CancelTurn aborts the in-flight agent turn (the equivalent of
	// pressing Esc in the cc TUI). Idempotent if no turn is in flight.
	CancelTurn(session *AgentSession) error

	// Events returns a long-lived channel of normalized agent events.
	// Closed when the session ends (Kill or upstream subprocess exit).
	// Implementations buffer events to avoid blocking the underlying
	// agent on slow consumers.
	Events(session *AgentSession) <-chan AgentEvent

	// SupportsHooks reports whether RespondHook is meaningful for this
	// provider. cc returns true; future providers without hooks return false.
	SupportsHooks() bool

	// HookCapabilities enumerates the hook events the provider can fire.
	// Returns an empty slice when SupportsHooks is false.
	HookCapabilities() []HookCap

	// RespondHook delivers a tether-side decision back to a hook awaiting
	// authorization. hookID is the identifier the provider attached to the
	// hook event when it fired (typically a request_id or UUID).
	RespondHook(hookID string, decision HookDecision) error

	// SupportedModes returns the permission modes accepted by SetMode.
	// cc: ["plan", "default", "auto", "bypassPermissions"].
	SupportedModes() []string

	// SetMode switches the agent's permission mode mid-session.
	SetMode(session *AgentSession, mode string) error
}

// AgentSession is the opaque per-session handle returned by Spawn / Resume
// and threaded through the rest of the AgentProvider methods. The ID field
// is provider-stable (cc populates it with the cc session_id once
// system/init lands). ProviderState is escape-hatched `any` so providers
// can stash whatever they need without leaking that type into the seam;
// concrete providers type-assert in each method (returning ErrInvalidSession
// on mismatch).
type AgentSession struct {
	ID            string
	ProviderState any
}

// AgentEvent is the normalized envelope every provider emits on its Events
// channel. The Payload preserves the provider-specific record bytes
// verbatim — daemon does not interpret it (spec §5.0 invariant: the wire
// layer above daemon may pass Payload through unchanged or decode it; the
// daemon itself stays content-agnostic).
type AgentEvent struct {
	UUID       string
	ParentUUID string
	EventType  EventType
	Payload    json.RawMessage
	Timestamp  time.Time
}

// EventType is the discriminator on AgentEvent. Constants below enumerate
// the v0.1 cc-derived value space; non-cc providers may emit subsets or
// supersets — the seam allows any string but consumers should treat
// unknown EventType as forwardable-but-undisplayed.
type EventType string

const (
	EventTypeSessionInit      EventType = "session-init"
	EventTypeUserMessage      EventType = "user-message"
	EventTypeAssistantMessage EventType = "assistant-message"
	EventTypeToolUse          EventType = "tool-use"
	EventTypeStreamEvent      EventType = "stream-event"
	EventTypeTurnResult       EventType = "turn-result"
	EventTypeRateLimit        EventType = "rate-limit"
)

// SpawnOpts collects the per-session parameters for AgentProvider.Spawn.
// Fields a provider doesn't understand must be silently ignored to keep
// daemon code provider-agnostic.
type SpawnOpts struct {
	// ProjectCwd is the working directory the agent runs in. Required.
	ProjectCwd string

	// Model is the optional model identifier (provider-specific naming).
	// cc accepts e.g. "haiku"; empty means provider default.
	Model string

	// InitialMessage seeds the first user turn. cc requires this because
	// stream-json mode does not emit system/init until the first user
	// envelope arrives on stdin (claude.Session.Start contract). Other
	// providers may treat empty as a no-op.
	InitialMessage string

	// ProviderConfig is a provider-specific escape hatch. Keys outside the
	// provider's known set must be silently ignored.
	ProviderConfig map[string]any

	// SettingsPath, when non-empty, points the agent at a daemon-owned
	// settings.json (cc: hook wiring + permission routing). Empty means
	// the agent reads its default user-level config — the legacy
	// behavior preserved for callers that don't run a daemon.
	//
	// For ClaudeCodeProvider this forwards to claude.SpawnOpts.SettingsPath
	// → cc's `--settings <path>` flag. Other providers may ignore.
	//
	// Callers (cmd/tether/*) typically populate this via
	// resolveCCSettings(); see cmd/tether/cc_settings.go for the
	// precedence chain. v0.1 daemon doesn't itself spawn cc — once it
	// does, this is the seam it'll use.
	SettingsPath string
}

// HookCap describes one hook event the provider can fire toward tether.
// EventName is the canonical name (cc uses "PreToolUse", "PostToolUse",
// etc.); InvokedAt is human-readable describing when the hook fires;
// Cancelable indicates whether the hook decision can deny the underlying
// action (true for PreToolUse / UserPromptSubmit; false for SessionStart /
// PostToolUse / Stop).
type HookCap struct {
	EventName  string
	InvokedAt  string
	Cancelable bool
}

// HookDecision is the tether-side reply to a hook awaiting authorization.
// Behavior is one of HookBehaviorAllow / Deny / Ask; Reason is surfaced in
// the UI / logs.
type HookDecision struct {
	Behavior string
	Reason   string
}

// HookBehavior values mirror cc's permissionDecision enum (spec §5.2 hook
// response schema F-05). Stable wire strings — do not rename.
const (
	HookBehaviorAllow = "allow"
	HookBehaviorDeny  = "deny"
	HookBehaviorAsk   = "ask"
)

// ErrNotImplemented is returned by methods a provider has not wired yet.
// Used by ClaudeCodeProvider for SetMode / RespondHook in PR-1; replaced
// with real implementations in PR-2 (Hook server) and PR-5 (daemon).
var ErrNotImplemented = errors.New("agent: not implemented in this provider")

// ErrInvalidSession is returned when a method receives an *AgentSession
// whose ProviderState doesn't match the receiver provider's expected type.
// Indicates a programming error: a session from one provider was passed to
// another.
var ErrInvalidSession = errors.New("agent: AgentSession.ProviderState type does not match this provider")
