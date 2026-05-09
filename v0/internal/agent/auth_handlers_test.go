package agent_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

func TestBrokerHookHandlers_PreToolUse_Allow(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)
	br.Timeout = 2 * time.Second

	envCh, cancel, _ := em.Subscribe("sid-h")
	defer cancel()

	// Auto-respond with allow-once when the request comes through.
	go func() {
		env := <-envCh
		rid, _ := env.PlaintextMetadata["requestId"].(string)
		// Verify the summary used a known field.
		summary, _ := env.PlaintextMetadata["summary"].(string)
		if !strings.Contains(summary, "Bash") || !strings.Contains(summary, "ls") {
			t.Errorf("summary missing tool/cmd: %q", summary)
		}
		_ = br.SubmitDecision(agent.AuthDecisionFrame{
			Type:      agent.KindAuthToolDecision,
			RequestID: rid,
			Decision:  agent.AuthDecisionAllowOnce,
		})
	}()

	h := agent.NewBrokerHookHandlers(br)
	payload := json.RawMessage(`{"session_id":"sid-h","tool_name":"Bash","tool_input":{"command":"ls"}}`)
	resp, err := h.PreToolUse(t.Context(), payload)
	if err != nil {
		t.Fatalf("PreToolUse: %v", err)
	}
	if resp.Decision != "allow" {
		t.Fatalf("decision: %q (want allow)", resp.Decision)
	}
}

func TestBrokerHookHandlers_PreToolUse_Deny(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)
	br.Timeout = 2 * time.Second

	envCh, cancel, _ := em.Subscribe("sid-d")
	defer cancel()

	go func() {
		env := <-envCh
		rid, _ := env.PlaintextMetadata["requestId"].(string)
		_ = br.SubmitDecision(agent.AuthDecisionFrame{
			Type:      agent.KindAuthToolDecision,
			RequestID: rid,
			Decision:  agent.AuthDecisionDenyOnce,
		})
	}()

	h := agent.NewBrokerHookHandlers(br)
	resp, _ := h.PreToolUse(t.Context(), json.RawMessage(`{"session_id":"sid-d","tool_name":"Bash"}`))
	if resp.Decision != "deny" {
		t.Fatalf("decision: %q (want deny)", resp.Decision)
	}
}

func TestBrokerHookHandlers_PreToolUse_MalformedPayload(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)

	h := agent.NewBrokerHookHandlers(br)
	resp, _ := h.PreToolUse(t.Context(), json.RawMessage(`not json`))
	if resp.Decision != "deny" {
		t.Fatalf("malformed payload should deny; got %q", resp.Decision)
	}
}

func TestBrokerHookHandlers_PreToolUse_MissingToolName(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)

	h := agent.NewBrokerHookHandlers(br)
	resp, _ := h.PreToolUse(t.Context(), json.RawMessage(`{"session_id":"x"}`))
	if resp.Decision != "deny" {
		t.Fatalf("missing tool_name should deny; got %q", resp.Decision)
	}
}

func TestBrokerHookHandlers_PreToolUse_NoBroker(t *testing.T) {
	t.Parallel()
	h := &agent.BrokerHookHandlers{Broker: nil}
	resp, _ := h.PreToolUse(t.Context(), json.RawMessage(`{"session_id":"x","tool_name":"Bash"}`))
	if resp.Decision != "deny" {
		t.Fatalf("nil broker should deny; got %q", resp.Decision)
	}
}
