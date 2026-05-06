package jsonl

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMap_Event_RoundTrip(t *testing.T) {
	line := []byte(`{"parentUuid":"p1","isSidechain":false,"type":"assistant","uuid":"a1","timestamp":"2026-04-30T06:55:01.000Z","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}

	env, ok := Map(rec, Classify(rec), MapOpts{})
	if !ok {
		t.Fatal("Map returned ok=false")
	}
	if env.Kind != KindAgentEvent {
		t.Errorf("Kind = %v, want %v", env.Kind, KindAgentEvent)
	}
	if env.ProviderType != ProviderTypeClaudeCode {
		t.Errorf("ProviderType = %q, want %q", env.ProviderType, ProviderTypeClaudeCode)
	}
	if env.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", env.SessionID)
	}
	if env.SourceUUID != "a1" {
		t.Errorf("SourceUUID = %q", env.SourceUUID)
	}
	if env.Class != ClassEvent {
		t.Errorf("Class = %v", env.Class)
	}
	if got := env.PlaintextMetadata["uuid"]; got != "a1" {
		t.Errorf("metadata.uuid = %v", got)
	}
	if got := env.PlaintextMetadata["parentUuid"]; got != "p1" {
		t.Errorf("metadata.parentUuid = %v", got)
	}
	if got := env.PlaintextMetadata["role"]; got != "assistant" {
		t.Errorf("metadata.role = %v", got)
	}
	// Payload should hold the cc message body verbatim.
	if !bytes.Contains(env.CiphertextPayload, []byte(`"role":"assistant"`)) {
		t.Errorf("payload missing message body: %s", string(env.CiphertextPayload))
	}
	if !bytes.Contains(env.CiphertextPayload, []byte(`"text":"hi"`)) {
		t.Errorf("payload missing text content: %s", string(env.CiphertextPayload))
	}
}

func TestMap_Hook(t *testing.T) {
	line := []byte(`{"isSidechain":false,"attachment":{"type":"hook_success","hookName":"PreToolUse:Read","hookEvent":"PreToolUse","toolUseID":"tu1","exitCode":0},"type":"attachment","uuid":"u1","sessionId":"sess-1"}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	env, ok := Map(rec, Classify(rec), MapOpts{})
	if !ok {
		t.Fatal("Map returned ok=false")
	}
	if env.Kind != KindHookEvent {
		t.Errorf("Kind = %v, want %v", env.Kind, KindHookEvent)
	}
	if env.Class != ClassHook {
		t.Errorf("Class = %v", env.Class)
	}
	if got := env.PlaintextMetadata["hookEvent"]; got != "PreToolUse" {
		t.Errorf("hookEvent = %v", got)
	}
	if got := env.PlaintextMetadata["hookName"]; got != "PreToolUse:Read" {
		t.Errorf("hookName = %v", got)
	}
	if !bytes.Contains(env.CiphertextPayload, []byte(`"hookEvent":"PreToolUse"`)) {
		t.Errorf("hook payload missing hookEvent: %s", string(env.CiphertextPayload))
	}
}

func TestMap_State_PermissionMode(t *testing.T) {
	line := []byte(`{"type":"permission-mode","permissionMode":"auto","sessionId":"sess-1"}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	env, ok := Map(rec, Classify(rec), MapOpts{})
	if !ok {
		t.Fatal("Map returned ok=false")
	}
	if env.Kind != KindSessionState {
		t.Errorf("Kind = %v, want session.state", env.Kind)
	}
	if got := env.PlaintextMetadata["recordType"]; got != "permission-mode" {
		t.Errorf("recordType = %v", got)
	}
	if got := env.PlaintextMetadata["permissionMode"]; got != "auto" {
		t.Errorf("permissionMode = %v", got)
	}
}

func TestMap_State_Unknown_RoutedAsState(t *testing.T) {
	line := []byte(`{"type":"future-thing","sessionId":"sess-1"}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	c := Classify(rec)
	if c != ClassUnknown {
		t.Fatalf("expected ClassUnknown, got %v", c)
	}
	env, ok := Map(rec, c, MapOpts{})
	if !ok {
		t.Fatal("Map returned ok=false")
	}
	if env.Kind != KindSessionState {
		t.Errorf("unknown should route to session.state, got %v", env.Kind)
	}
	if env.Class != ClassUnknown {
		t.Errorf("Class should preserve UNKNOWN for metrics, got %v", env.Class)
	}
}

func TestMarshalEnvelope(t *testing.T) {
	env := Envelope{
		Kind:              KindAgentEvent,
		ProviderType:      ProviderTypeClaudeCode,
		SessionID:         "sess-1",
		PlaintextMetadata: map[string]any{"uuid": "u1"},
		CiphertextPayload: json.RawMessage(`{"hello":"world"}`),
		Class:             ClassEvent,
		SourceUUID:        "u1",
	}
	b, err := MarshalEnvelope(env)
	if err != nil {
		t.Fatalf("MarshalEnvelope: %v", err)
	}
	if !bytes.Contains(b, []byte(`"kind": "output.agent-event"`)) {
		t.Errorf("missing kind: %s", string(b))
	}
}

// --- fence-tag seam (sub-task #11) ---

func TestExtractFenceTag_WithSkill(t *testing.T) {
	cases := []struct {
		line, wantType, wantSkill string
		ok                        bool
	}{
		{"```dag:writing-plans", "dag", "writing-plans", true},
		{"```form:onboarding", "form", "onboarding", true},
		{"```media:xiyou-animation", "media", "xiyou-animation", true},
		{"```candidates:agent-pick", "candidates", "agent-pick", true},
		{"```dag", "dag", "unknown", true}, // missing :skill → "unknown" per §11.Z.10
		{"```python", "", "", false},        // not a tether block-type
		{"   ```dag:foo   ", "dag", "foo", true},
		{"prefix ```dag:foo", "", "", false}, // anchored at line start
		{"```", "", "", false},
	}
	for _, c := range cases {
		bt, sk, ok := extractFenceTag(c.line)
		if ok != c.ok || bt != c.wantType || sk != c.wantSkill {
			t.Errorf("extractFenceTag(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.line, bt, sk, ok, c.wantType, c.wantSkill, c.ok)
		}
	}
}

func TestMap_FenceTagGrep_Off_NoMetadata(t *testing.T) {
	// v0.1 default: FenceTagGrep is off. Even with a fence in the
	// content, no blockType / skillName attaches to envelope.
	line := []byte(`{"type":"assistant","uuid":"a1","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"see\n` + "```" + `dag:writing-plans\nv:1\n` + "```" + `\n"}]}}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	env, _ := Map(rec, Classify(rec), MapOpts{FenceTagGrep: false})
	if _, has := env.PlaintextMetadata["blockType"]; has {
		t.Errorf("blockType must NOT leak when FenceTagGrep=false")
	}
}

func TestMap_FenceTagGrep_On_AttachesMetadata(t *testing.T) {
	// Sub-task #11 path: when FenceTagGrep flips on, blockType +
	// skillName attach to plaintext metadata.
	line := []byte(`{"type":"assistant","uuid":"a1","sessionId":"sess-1","message":{"role":"assistant","content":[{"type":"text","text":"see\n` + "```" + `dag:writing-plans\nv:1\n` + "```" + `\n"}]}}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	env, _ := Map(rec, Classify(rec), MapOpts{FenceTagGrep: true})
	if got := env.PlaintextMetadata["blockType"]; got != "dag" {
		t.Errorf("blockType = %v, want dag", got)
	}
	if got := env.PlaintextMetadata["skillName"]; got != "writing-plans" {
		t.Errorf("skillName = %v, want writing-plans", got)
	}
	if env.Skill != "writing-plans" {
		t.Errorf("env.Skill = %q, want writing-plans", env.Skill)
	}
}
