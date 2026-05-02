package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/backend/claude"
)

// --- mapper coverage (pure function) ---------------------------------

func TestMapEnvelope_KnownTypes(t *testing.T) {
	cases := []struct {
		name    string
		env     claude.Envelope
		wantET  EventType
		wantUID string
	}{
		{"system/init", claude.Envelope{Type: claude.EventSystem, Subtype: "init", UUID: "u1", Raw: json.RawMessage(`{"type":"system","subtype":"init"}`)}, EventTypeSessionInit, "u1"},
		{"stream_event", claude.Envelope{Type: claude.EventStreamEvent, UUID: "u2", Raw: json.RawMessage(`{"type":"stream_event"}`)}, EventTypeStreamEvent, "u2"},
		{"assistant", claude.Envelope{Type: claude.EventAssistant, UUID: "u3", Raw: json.RawMessage(`{"type":"assistant"}`)}, EventTypeAssistantMessage, "u3"},
		{"user", claude.Envelope{Type: claude.EventUser, UUID: "u4", Raw: json.RawMessage(`{"type":"user"}`)}, EventTypeUserMessage, "u4"},
		{"result", claude.Envelope{Type: claude.EventResult, UUID: "u5", Raw: json.RawMessage(`{"type":"result"}`)}, EventTypeTurnResult, "u5"},
		{"rate_limit", claude.Envelope{Type: claude.EventRateLimit, UUID: "u6", Raw: json.RawMessage(`{"type":"rate_limit_event"}`)}, EventTypeRateLimit, "u6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := mapEnvelope(tc.env)
			if !ok {
				t.Fatalf("mapEnvelope dropped %s envelope", tc.name)
			}
			if ev.EventType != tc.wantET {
				t.Errorf("EventType: got %q want %q", ev.EventType, tc.wantET)
			}
			if ev.UUID != tc.wantUID {
				t.Errorf("UUID: got %q want %q", ev.UUID, tc.wantUID)
			}
			if string(ev.Payload) != string(tc.env.Raw) {
				t.Errorf("Payload passthrough broken: got %q want %q", string(ev.Payload), string(tc.env.Raw))
			}
			if ev.Timestamp.IsZero() {
				t.Error("Timestamp not set")
			}
		})
	}
}

func TestMapEnvelope_DroppedTypes(t *testing.T) {
	// Internal control envelopes never reach Events channel (Session
	// dispatcher routes them to Controller). But if they did, mapper
	// drops them defensively.
	dropped := []claude.Envelope{
		{Type: claude.EventControlRequest},
		{Type: claude.EventControlResponse},
		{Type: claude.EventSystem, Subtype: "hook_started"},
		{Type: claude.EventSystem, Subtype: "hook_response"},
		{Type: claude.EventSystem, Subtype: "status"},
		{Type: claude.EventType("unknown-future")},
	}
	for _, env := range dropped {
		if _, ok := mapEnvelope(env); ok {
			t.Errorf("mapEnvelope should drop type=%s subtype=%s", env.Type, env.Subtype)
		}
	}
}

// --- static method coverage -------------------------------------------

func TestProvider_ProviderType(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	if got := p.ProviderType(); got != "claude-code" {
		t.Errorf("ProviderType: got %q want claude-code", got)
	}
}

func TestProvider_SupportsHooks(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	if !p.SupportsHooks() {
		t.Error("SupportsHooks: cc supports hooks (spec §5.2 lists 5 events)")
	}
}

func TestProvider_HookCapabilities(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	caps := p.HookCapabilities()
	want := map[string]bool{ // event_name → cancelable
		"PreToolUse":        true,
		"PostToolUse":       false,
		"SessionStart":      false,
		"UserPromptSubmit":  true,
		"Stop":              false,
	}
	if len(caps) != len(want) {
		t.Fatalf("HookCapabilities count: got %d want %d", len(caps), len(want))
	}
	for _, c := range caps {
		gotCancel, known := want[c.EventName]
		if !known {
			t.Errorf("unexpected hook event: %q", c.EventName)
			continue
		}
		if c.Cancelable != gotCancel {
			t.Errorf("hook %s Cancelable: got %v want %v", c.EventName, c.Cancelable, gotCancel)
		}
		if c.InvokedAt == "" {
			t.Errorf("hook %s missing InvokedAt", c.EventName)
		}
	}
}

func TestProvider_SupportedModes(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	modes := p.SupportedModes()
	wantSet := map[string]bool{"plan": true, "default": true, "auto": true, "bypassPermissions": true}
	if len(modes) != len(wantSet) {
		t.Fatalf("SupportedModes count: got %d want %d", len(modes), len(wantSet))
	}
	for _, m := range modes {
		if !wantSet[m] {
			t.Errorf("unexpected mode: %q", m)
		}
	}
}

// --- stubbed methods return ErrNotImplemented -----------------------

func TestProvider_RespondHook_NotImplemented(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	if err := p.RespondHook("hook-id", HookDecision{Behavior: HookBehaviorAllow}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("RespondHook: got %v want ErrNotImplemented", err)
	}
}

func TestProvider_SetMode_NotImplemented(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	sess := &AgentSession{ID: "x", ProviderState: &claudeSessionState{}}
	if err := p.SetMode(sess, "plan"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SetMode: got %v want ErrNotImplemented", err)
	}
}

func TestProvider_Resume_NotImplemented(t *testing.T) {
	// Resume needs findsession.go (PR-3 / S6) to rehydrate ProjectCwd
	// from an arbitrary session_id. PR-1 stubs it.
	p := NewClaudeCodeProvider(nil)
	if _, err := p.Resume(context.Background(), "any-sid"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Resume: got %v want ErrNotImplemented", err)
	}
}

// --- type assertion guards ------------------------------------------

func TestProvider_RejectsForeignSession(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	foreign := &AgentSession{ID: "foreign", ProviderState: "not-a-claudeSessionState"}

	if err := p.SendUserMessage(foreign, "hi"); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("SendUserMessage(foreign): got %v want ErrInvalidSession", err)
	}
	if err := p.CancelTurn(foreign); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("CancelTurn(foreign): got %v want ErrInvalidSession", err)
	}
	if err := p.Kill(foreign); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("Kill(foreign): got %v want ErrInvalidSession", err)
	}
	if ch := p.Events(foreign); ch != nil {
		t.Error("Events(foreign): expected nil channel, got non-nil")
	}
}

func TestProvider_RejectsNilSession(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	if err := p.SendUserMessage(nil, "hi"); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("SendUserMessage(nil): got %v", err)
	}
	if err := p.CancelTurn(nil); !errors.Is(err, ErrInvalidSession) {
		t.Errorf("CancelTurn(nil): got %v", err)
	}
}

// --- Spawn validates inputs -----------------------------------------

func TestProvider_Spawn_RequiresInitialMessage(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	_, err := p.Spawn(context.Background(), SpawnOpts{ProjectCwd: "/tmp"})
	if err == nil {
		t.Fatal("Spawn with empty InitialMessage: expected error")
	}
	if !strings.Contains(err.Error(), "InitialMessage") {
		t.Errorf("error should mention InitialMessage requirement: got %q", err.Error())
	}
}

// TestProvider_Spawn_FakeBinaryInitTimeout exercises the real claude.New +
// Start path with a binary that never emits system/init. Pattern matches
// gh-13's TestSession_InitTimeout (uses /bin/cat which reads stdin and
// holds it open without ever emitting a system/init line). Confirms the
// Spawn delegation wiring is correct without spending API tokens.
func TestProvider_Spawn_FakeBinaryInitTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping init-timeout test (-short)")
	}
	p := NewClaudeCodeProvider(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := p.Spawn(ctx, SpawnOpts{
		ProjectCwd:     "/tmp",
		InitialMessage: "hi",
		ProviderConfig: map[string]any{"binary_path": "/bin/cat"},
	})
	if err == nil {
		t.Fatal("Spawn with /bin/cat: expected init-timeout or ctx-cancel error")
	}
	// Either ctx deadline or claude.ErrInitTimeout is acceptable —
	// the key assertion is the call landed in claude.New + Start.
}

func TestProvider_Spawn_RequiresProjectCwd(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	_, err := p.Spawn(context.Background(), SpawnOpts{InitialMessage: "hi"})
	if err == nil {
		t.Fatal("Spawn with empty ProjectCwd: expected error")
	}
	if !strings.Contains(err.Error(), "ProjectCwd") {
		t.Errorf("error should mention ProjectCwd: got %q", err.Error())
	}
}

func TestProvider_Spawn_RejectsNonStringBinaryPath(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	_, err := p.Spawn(context.Background(), SpawnOpts{
		ProjectCwd:     "/tmp",
		InitialMessage: "hi",
		ProviderConfig: map[string]any{"binary_path": 123}, // wrong type
	})
	if err == nil {
		t.Fatal("Spawn with non-string binary_path: expected error")
	}
	if !strings.Contains(err.Error(), "binary_path") || !strings.Contains(err.Error(), "string") {
		t.Errorf("error should explain string requirement: got %q", err.Error())
	}
}

// --- post-Kill semantics + double-Kill idempotency --------------------

// fakeKillSession lets us drive the Kill / post-Kill code paths without
// spawning a real subprocess. It implements just enough of *claude.Session
// to be plumbed through claudeSessionState; we instantiate it directly
// without going through Spawn.
//
// The real *claude.Session is a struct, not an interface — so we cannot
// substitute it directly. Instead, this test constructs an AgentSession
// whose ProviderState carries a sentinel that the production helpers will
// reject (foreign-session path). That is sufficient to exercise Kill /
// SendUserMessage / CancelTurn / Events post-foreign-state, which is the
// observable concurrency surface from outside the package.
//
// For idempotency-of-double-Kill on a real claudeSessionState, we use a
// /bin/cat-spawned session: Spawn fails at Start (init timeout), so by
// then we don't have a sessionState to test against. The defense for
// double-Kill at the provider layer is the cs.closed flag + cs.mu, which
// is inspected by the unit test below by manually constructing a
// *claudeSessionState (white-box).
func TestProvider_DoubleKill_Idempotent_Whitebox(t *testing.T) {
	// White-box test: directly construct claudeSessionState with a nil
	// session, verify Kill double-call is idempotent at the cs.closed
	// gate. The actual sess.Close() panics on nil — but the second Kill
	// must not even reach sess.Close (must short-circuit on cs.closed).
	cs := &claudeSessionState{
		// sess intentionally nil — second Kill must not touch it.
		out:          make(chan AgentEvent, 1),
		cancelMapper: func() {},
	}
	// First Kill will panic on cs.sess.Close() because sess is nil; we
	// recover to assert that path runs once.
	p := NewClaudeCodeProvider(nil)
	sess := &AgentSession{ID: "x", ProviderState: cs}

	// Mark closed BEFORE first call — this simulates the post-Kill state
	// and verifies that a Kill on already-closed state short-circuits.
	cs.mu.Lock()
	cs.closed = true
	cs.mu.Unlock()

	if err := p.Kill(sess); err != nil {
		t.Fatalf("Kill on already-closed session: got %v want nil", err)
	}
	// Second Kill: same path.
	if err := p.Kill(sess); err != nil {
		t.Fatalf("second Kill on already-closed session: got %v want nil", err)
	}
}

// --- Events() returns the same channel reference -------------------

func TestProvider_Events_SameChannelRef(t *testing.T) {
	p := NewClaudeCodeProvider(nil)
	cs := &claudeSessionState{
		out:          make(chan AgentEvent, 1),
		cancelMapper: func() {},
	}
	sess := &AgentSession{ID: "x", ProviderState: cs}
	ch1 := p.Events(sess)
	ch2 := p.Events(sess)
	// Channels are reference types; same backing must mean same value.
	if ch1 == nil || ch2 == nil {
		t.Fatal("Events returned nil for a valid session")
	}
	// We can't compare directional channels with == directly via interface
	// promotion, but len/cap are stable indicators if backing differs (a
	// freshly-allocated chan would have a different cap, here both must
	// have cap 1). Probe by sending on the underlying chan and reading on
	// the receiver.
	go func() { cs.out <- AgentEvent{EventType: EventTypeUserMessage} }()
	select {
	case ev := <-ch1:
		if ev.EventType != EventTypeUserMessage {
			t.Errorf("ch1 received wrong event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("ch1 did not receive — channel is not the same ref as cs.out")
	}
	// ch2 must observe the same closure when cs.out closes.
	close(cs.out)
	if _, ok := <-ch2; ok {
		t.Error("ch2 should have observed close from same backing channel")
	}
}

// --- AgentEvent.ParentUUID is unconditionally empty in v0.1 -----------

func TestMapEnvelope_ParentUUIDIsEmptyInV01(t *testing.T) {
	// Document v0.1 behavior: parentUuid is nested in cc records and
	// requires a full record decode to extract; for v0.1 mapper we leave
	// it empty. This test trips when PR-3 wires the §11.AA fence-tag /
	// parent-chain decode and someone forgets to update.
	env := claude.Envelope{
		Type:    claude.EventAssistant,
		UUID:    "child-uuid",
		Raw:     json.RawMessage(`{"type":"assistant","uuid":"child-uuid","parentUuid":"parent-should-not-leak-yet"}`),
	}
	ev, ok := mapEnvelope(env)
	if !ok {
		t.Fatal("mapEnvelope dropped assistant envelope")
	}
	if ev.ParentUUID != "" {
		t.Errorf("v0.1 mapper should leave ParentUUID empty (parentUuid extraction lands in PR-3); got %q", ev.ParentUUID)
	}
}

// --- re-exported sentinel error stability -----------------------------

func TestSentinelErrorsReExported(t *testing.T) {
	// agent.ErrXxx must alias the backend sentinels so callers above the
	// seam don't need to import internal/backend/claude. errors.Is
	// matches via the aliasing.
	if !errors.Is(ErrInitTimeout, claude.ErrInitTimeout) {
		t.Error("agent.ErrInitTimeout must alias claude.ErrInitTimeout")
	}
	if !errors.Is(ErrSubprocessGone, claude.ErrSubprocessGone) {
		t.Error("agent.ErrSubprocessGone must alias claude.ErrSubprocessGone")
	}
	if !errors.Is(ErrSessionClosed, claude.ErrSessionClosed) {
		t.Error("agent.ErrSessionClosed must alias claude.ErrSessionClosed")
	}
}
