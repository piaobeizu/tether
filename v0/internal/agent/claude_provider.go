package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/piaobeizu/tether/internal/backend/claude"
)

// Re-exported sentinel errors from the wrapped backend. Daemon code paths
// must not import internal/backend/claude directly (D-17 seam invariant);
// callers above this layer match against agent.ErrInitTimeout etc. via
// errors.Is. The aliases are stable across cc backend swaps in v0.1 (only
// one backend exists); when other AgentProviders join in v0.2+, each may
// define its own sentinel set in this package or surface its own.
var (
	// ErrInitTimeout is returned when the agent backend does not emit its
	// init signal within the per-backend timeout (cc: 10s default).
	ErrInitTimeout = claude.ErrInitTimeout
	// ErrSubprocessGone is returned by SendUserMessage when stdin write
	// fails because the agent subprocess has exited.
	ErrSubprocessGone = claude.ErrSubprocessGone
	// ErrSessionClosed is returned when operations are attempted on a
	// session that has already been Kill'd.
	ErrSessionClosed = claude.ErrSessionClosed
)

// ClaudeCodeProvider is the v0.1 single AgentProvider implementation,
// wrapping internal/backend/claude.Session. Daemon code paths address it
// only through the AgentProvider interface (D-17 seam); the concrete type
// is exposed only so callers can construct it.
type ClaudeCodeProvider struct {
	auth claude.ToolAuthorizer
}

// NewClaudeCodeProvider constructs a provider that uses the given
// ToolAuthorizer for cc's can_use_tool flow. Pass nil to deny all tool
// authorization at the provider level (the cc TUI / hook layer is the
// fallback authority).
func NewClaudeCodeProvider(auth claude.ToolAuthorizer) *ClaudeCodeProvider {
	return &ClaudeCodeProvider{auth: auth}
}

// claudeSessionState is what we stash inside AgentSession.ProviderState.
// Holds the live cc Session + its derived AgentEvent channel + the cancel
// func that tears down the mapper goroutine.
type claudeSessionState struct {
	sess         *claude.Session
	out          chan AgentEvent
	cancelMapper context.CancelFunc

	mu     sync.Mutex
	closed bool
}

// session extracts and validates the per-session state. Returns
// ErrInvalidSession on nil session or wrong ProviderState type.
func (p *ClaudeCodeProvider) session(s *AgentSession) (*claudeSessionState, error) {
	if s == nil {
		return nil, ErrInvalidSession
	}
	cs, ok := s.ProviderState.(*claudeSessionState)
	if !ok {
		return nil, ErrInvalidSession
	}
	return cs, nil
}

// --- AgentProvider interface --------------------------------------------

func (p *ClaudeCodeProvider) ProviderType() string { return "claude-code" }

func (p *ClaudeCodeProvider) Spawn(ctx context.Context, opts SpawnOpts) (*AgentSession, error) {
	if opts.ProjectCwd == "" {
		return nil, errors.New("claude-code: SpawnOpts.ProjectCwd required (cc buckets sessions by cwd; empty would silently land in $HOME)")
	}
	if opts.InitialMessage == "" {
		return nil, errors.New("claude-code: SpawnOpts.InitialMessage required (cc emits system/init only after first user envelope; see claude.Session.Start contract)")
	}
	binPath, err := extractBinaryPath(opts.ProviderConfig)
	if err != nil {
		return nil, err
	}

	sess, err := claude.New(ctx, claude.SpawnOpts{
		ProjectCwd:   opts.ProjectCwd,
		Model:        opts.Model,
		BinaryPath:   binPath,
		SettingsPath: opts.SettingsPath,
	}, p.auth)
	if err != nil {
		return nil, err
	}
	if err := sess.Start(ctx, opts.InitialMessage); err != nil {
		_ = sess.Close()
		return nil, err
	}

	// Mapper ctx is rooted at Background, NOT the caller's ctx. The caller's
	// ctx scopes the Spawn RPC; the mapper goroutine and subprocess outlive
	// the RPC by design. Tearing the mapper down on the spawn ctx would
	// orphan cs.sess (cc subprocess keeps running) and falsely signal
	// session-ended to Events() consumers via a closed channel. Tear down
	// happens explicitly in Kill (cancelMapper + sess.Close) or implicitly
	// when sess.Events() closes after upstream subprocess exit.
	mapperCtx, cancel := context.WithCancel(context.Background())
	cs := &claudeSessionState{
		sess:         sess,
		out:          make(chan AgentEvent, 64),
		cancelMapper: cancel,
	}
	go runMapper(mapperCtx, sess.Events(), cs.out)

	return &AgentSession{
		ID:            sess.SessionID(),
		ProviderState: cs,
	}, nil
}

// extractBinaryPath validates and returns ProviderConfig["binary_path"]. The
// key is optional (empty string falls back to PATH lookup), but if present
// it must be a string — silent type-assertion drop would mask caller bugs.
func extractBinaryPath(cfg map[string]any) (string, error) {
	v, ok := cfg["binary_path"]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("claude-code: ProviderConfig.binary_path must be string, got %T", v)
	}
	return s, nil
}

// Resume is stubbed in PR-1: cold-start by sid alone requires findsession.go
// (PR-3 / S6) to rehydrate ProjectCwd from cc's bucket. Real Resume lands
// when that ships.
func (p *ClaudeCodeProvider) Resume(_ context.Context, _ string) (*AgentSession, error) {
	return nil, ErrNotImplemented
}

func (p *ClaudeCodeProvider) Kill(s *AgentSession) error {
	cs, err := p.session(s)
	if err != nil {
		return err
	}
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		return nil
	}
	cs.closed = true
	cs.mu.Unlock()

	cs.cancelMapper()        // stop the mapper goroutine
	return cs.sess.Close()   // SIGTERM cc (claude.Session.Close uses gracefulTerminate, NOT SIGINT — F-07)
}

func (p *ClaudeCodeProvider) SendUserMessage(s *AgentSession, text string) error {
	cs, err := p.session(s)
	if err != nil {
		return err
	}
	return cs.sess.Send(text)
}

func (p *ClaudeCodeProvider) CancelTurn(s *AgentSession) error {
	cs, err := p.session(s)
	if err != nil {
		return err
	}
	return cs.sess.SendInterrupt(context.Background())
}

func (p *ClaudeCodeProvider) Events(s *AgentSession) <-chan AgentEvent {
	cs, err := p.session(s)
	if err != nil {
		return nil
	}
	return cs.out
}

func (p *ClaudeCodeProvider) SupportsHooks() bool { return true }

// HookCapabilities enumerates cc's 5 hook events per spec §5.2 / F-05.
// EventNames are cc-canonical (PreToolUse, etc.); InvokedAt is human prose;
// Cancelable reflects whether cc honors a "deny" decision (true for the
// two pre-action hooks, false for observers).
func (p *ClaudeCodeProvider) HookCapabilities() []HookCap {
	return []HookCap{
		{EventName: "PreToolUse", InvokedAt: "before tool execution; can deny", Cancelable: true},
		{EventName: "PostToolUse", InvokedAt: "after tool execution; observer only", Cancelable: false},
		{EventName: "SessionStart", InvokedAt: "session bootstrap; observer only", Cancelable: false},
		{EventName: "UserPromptSubmit", InvokedAt: "before user prompt routes to LLM; can deny", Cancelable: true},
		{EventName: "Stop", InvokedAt: "turn complete; observer only", Cancelable: false},
	}
}

// RespondHook is stubbed in PR-1; real wire-up arrives in PR-2 when the
// internal/cc/hookserver lands the HTTP handler that needs to deliver
// decisions back to cc via its callback contract.
func (p *ClaudeCodeProvider) RespondHook(_ string, _ HookDecision) error {
	return ErrNotImplemented
}

// SupportedModes returns cc's permission_mode enum per spec §5.0 / §5.5.
// Order is stable but not semantically meaningful; consumers should treat
// the result as a set.
func (p *ClaudeCodeProvider) SupportedModes() []string {
	return []string{"plan", "default", "auto", "bypassPermissions"}
}

// SetMode is stubbed in PR-1; real wire-up arrives in PR-5 when the daemon
// orchestration layer can route a runtime mode switch through cc's
// /mode-* slash commands or the equivalent control_request path.
func (p *ClaudeCodeProvider) SetMode(_ *AgentSession, _ string) error {
	return ErrNotImplemented
}

// --- mapper -------------------------------------------------------------

// mapEnvelope projects a claude.Envelope into an AgentEvent, returning
// (_, false) when the envelope is internal (control_*) or unknown — the
// caller drops it from the Events channel.
//
// Timestamp is set at mapping time (now), not extracted from the envelope:
// most cc record types don't carry a wire timestamp, and consumers care
// about ingest time anyway.
//
// ParentUUID is left empty in v0.1 — cc's NDJSON parentUuid field is
// nested inside the assistant/user content and reading it requires a full
// record decode. Defer until a consumer needs the parent chain (likely
// alongside §11.AA fence tag work in PR-3).
func mapEnvelope(env claude.Envelope) (AgentEvent, bool) {
	var et EventType
	switch env.Type {
	case claude.EventSystem:
		// Only system/init becomes a normalized AgentEvent in v0.1.
		// Other system subtypes (hook_started/hook_response/status) are
		// internal and consumed by daemon-side state machines, not the
		// AgentEvent stream.
		if env.Subtype == "init" {
			et = EventTypeSessionInit
		} else {
			return AgentEvent{}, false
		}
	case claude.EventStreamEvent:
		et = EventTypeStreamEvent
	case claude.EventAssistant:
		et = EventTypeAssistantMessage
	case claude.EventUser:
		et = EventTypeUserMessage
	case claude.EventResult:
		et = EventTypeTurnResult
	case claude.EventRateLimit:
		et = EventTypeRateLimit
	default:
		// control_request / control_response are intercepted by Session's
		// dispatcher and never reach Events(). Any other unknown type is
		// dropped defensively.
		return AgentEvent{}, false
	}
	return AgentEvent{
		UUID:      env.UUID,
		EventType: et,
		Payload:   env.Raw,
		Timestamp: time.Now(),
	}, true
}

// runMapper bridges the cc envelope channel to the agent-side AgentEvent
// channel. Closes out when src closes or ctx is canceled.
func runMapper(ctx context.Context, src <-chan claude.Envelope, out chan<- AgentEvent) {
	defer close(out)
	for {
		select {
		case env, ok := <-src:
			if !ok {
				return
			}
			ev, keep := mapEnvelope(env)
			if !keep {
				continue
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
