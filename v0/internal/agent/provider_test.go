package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// nilProvider is a compile-time interface conformance witness. If
// AgentProvider's surface changes, this stub stops compiling.
type nilProvider struct{}

func (nilProvider) ProviderType() string { return "nil" }

func (nilProvider) Spawn(_ context.Context, _ agent.SpawnOpts) (*agent.AgentSession, error) {
	return nil, errors.New("nil")
}

func (nilProvider) Resume(_ context.Context, _ string) (*agent.AgentSession, error) {
	return nil, errors.New("nil")
}

func (nilProvider) Kill(_ *agent.AgentSession) error                          { return nil }
func (nilProvider) SendUserMessage(_ *agent.AgentSession, _ string) error     { return nil }
func (nilProvider) CancelTurn(_ *agent.AgentSession) error                    { return nil }
func (nilProvider) Events(_ *agent.AgentSession) <-chan agent.AgentEvent      { return nil }
func (nilProvider) SupportsHooks() bool                                       { return false }
func (nilProvider) HookCapabilities() []agent.HookCap                         { return nil }
func (nilProvider) RespondHook(_ string, _ agent.HookDecision) error          { return nil }
func (nilProvider) SupportedModes() []string                                  { return nil }
func (nilProvider) SetMode(_ *agent.AgentSession, _ string) error             { return nil }

var _ agent.AgentProvider = (*nilProvider)(nil)

func TestAgentEventShape(t *testing.T) {
	now := time.Now()
	ev := agent.AgentEvent{
		UUID:       "abc",
		ParentUUID: "parent",
		EventType:  agent.EventTypeUserMessage,
		Payload:    json.RawMessage(`{"role":"user"}`),
		Timestamp:  now,
	}
	if ev.UUID != "abc" {
		t.Fatalf("UUID round-trip: got %q", ev.UUID)
	}
	if ev.EventType != "user-message" {
		t.Fatalf("EventTypeUserMessage const: got %q want %q", ev.EventType, "user-message")
	}
	if !ev.Timestamp.Equal(now) {
		t.Fatalf("Timestamp round-trip mismatch")
	}
	if string(ev.Payload) != `{"role":"user"}` {
		t.Fatalf("Payload preserved: got %q", string(ev.Payload))
	}
}

func TestAgentEventTypeConstants(t *testing.T) {
	// Constants must be stable wire strings — downstream consumers grep them.
	cases := map[string]string{
		string(agent.EventTypeSessionInit):      "session-init",
		string(agent.EventTypeUserMessage):      "user-message",
		string(agent.EventTypeAssistantMessage): "assistant-message",
		string(agent.EventTypeToolUse):          "tool-use",
		string(agent.EventTypeStreamEvent):      "stream-event",
		string(agent.EventTypeTurnResult):       "turn-result",
		string(agent.EventTypeRateLimit):        "rate-limit",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("EventType constant: got %q want %q", got, want)
		}
	}
}

func TestHookCapShape(t *testing.T) {
	hc := agent.HookCap{
		EventName:  "PreToolUse",
		InvokedAt:  "before tool execution",
		Cancelable: true,
	}
	if hc.EventName != "PreToolUse" || !hc.Cancelable {
		t.Fatalf("HookCap fields: got %+v", hc)
	}
}

func TestHookDecisionShape(t *testing.T) {
	hd := agent.HookDecision{
		Behavior: agent.HookBehaviorAllow,
		Reason:   "auto-approved",
	}
	if hd.Behavior != "allow" {
		t.Fatalf("HookBehaviorAllow const: got %q", hd.Behavior)
	}
	hd2 := agent.HookDecision{Behavior: agent.HookBehaviorDeny}
	if hd2.Behavior != "deny" {
		t.Fatalf("HookBehaviorDeny const: got %q", hd2.Behavior)
	}
	hd3 := agent.HookDecision{Behavior: agent.HookBehaviorAsk}
	if hd3.Behavior != "ask" {
		t.Fatalf("HookBehaviorAsk const: got %q", hd3.Behavior)
	}
}

func TestAgentSessionShape(t *testing.T) {
	type providerSpecific struct{ secret int }
	s := &agent.AgentSession{
		ID:            "sid-123",
		ProviderState: &providerSpecific{secret: 42},
	}
	if s.ID != "sid-123" {
		t.Fatalf("ID round-trip: got %q", s.ID)
	}
	got, ok := s.ProviderState.(*providerSpecific)
	if !ok || got.secret != 42 {
		t.Fatalf("ProviderState type-assert: got %+v ok=%v", got, ok)
	}
}

func TestSpawnOptsShape(t *testing.T) {
	opts := agent.SpawnOpts{
		ProjectCwd:     "/tmp/x",
		Model:          "haiku",
		InitialMessage: "hi",
		ProviderConfig: map[string]any{"k": "v"},
	}
	if opts.ProjectCwd != "/tmp/x" || opts.ProviderConfig["k"] != "v" {
		t.Fatalf("SpawnOpts fields: got %+v", opts)
	}
}

// TestErrNotImplementedStable guards the sentinel error string — callers
// may match by errors.Is or by string-equal in logs.
func TestErrNotImplementedStable(t *testing.T) {
	if agent.ErrNotImplemented.Error() != "agent: not implemented in this provider" {
		t.Fatalf("ErrNotImplemented text changed: %q", agent.ErrNotImplemented.Error())
	}
}

// TestErrInvalidSessionStable guards the type-assertion failure sentinel.
func TestErrInvalidSessionStable(t *testing.T) {
	if agent.ErrInvalidSession.Error() != "agent: AgentSession.ProviderState type does not match this provider" {
		t.Fatalf("ErrInvalidSession text changed: %q", agent.ErrInvalidSession.Error())
	}
}
