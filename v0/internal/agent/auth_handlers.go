// Package agent — adapter that wires AuthBroker into cc.HookHandlers.
//
// The cc/hookserver expects a HookHandlers interface; the broker has
// the broker-shaped Ask() API. This file translates cc PreToolUse JSON
// payloads → broker Ask() calls and the resulting AuthDecision back
// into the cc allow/deny string.

package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/piaobeizu/tether/internal/cc"
)

// BrokerHookHandlers implements cc.HookHandlers by delegating
// PreToolUse to an AuthBroker. The other four hooks fall through to a
// permissive observer (PostToolUse / SessionStart / Stop are pure
// observers; UserPromptSubmit returns "allow" — v0.1 has no
// user-prompt authz UX).
//
// Wrap with NewBrokerHookHandlers; pass to cc.NewHookServer.
type BrokerHookHandlers struct {
	Broker *AuthBroker
}

// NewBrokerHookHandlers returns a HookHandlers that uses broker for
// PreToolUse decisions.
func NewBrokerHookHandlers(b *AuthBroker) *BrokerHookHandlers {
	return &BrokerHookHandlers{Broker: b}
}

// preToolUsePayload is the subset of cc's PreToolUse JSON we read. We
// keep this internal — the daemon's seam invariant is "don't interpret
// event content"; the only fields here are the ones we need to label
// the UI prompt.
type preToolUsePayload struct {
	SessionID string          `json:"session_id"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Input     json.RawMessage `json:"input"` // older cc payload variant
}

// PreToolUse asks the broker; surfaces decision back to cc.
func (h *BrokerHookHandlers) PreToolUse(ctx context.Context, payload json.RawMessage) (cc.HookResponse, error) {
	if h.Broker == nil {
		// Fail-closed: no broker means no UX surface to consult.
		return cc.HookResponse{Decision: "deny", Reason: "tether: no auth broker wired"}, nil
	}
	var p preToolUsePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return cc.HookResponse{Decision: "deny", Reason: "tether: malformed PreToolUse payload"}, nil
	}
	if p.ToolName == "" {
		return cc.HookResponse{Decision: "deny", Reason: "tether: PreToolUse missing tool_name"}, nil
	}
	input := p.ToolInput
	if len(input) == 0 {
		input = p.Input
	}
	summary := summarizeToolInput(p.ToolName, input)

	d, err := h.Broker.Ask(ctx, p.SessionID, p.ToolName, input, summary)
	if err != nil {
		// Includes timeout + ctx cancel. Decision is already deny on
		// these paths; surface the reason for cc UI.
		return cc.HookResponse{Decision: "deny", Reason: fmt.Sprintf("tether: %s", err.Error())}, nil
	}
	if d.IsAllow() {
		return cc.HookResponse{Decision: "allow", Reason: "tether: user authorized"}, nil
	}
	return cc.HookResponse{Decision: "deny", Reason: "tether: user denied"}, nil
}

// PostToolUse is a pure observer.
func (*BrokerHookHandlers) PostToolUse(_ context.Context, _ json.RawMessage) error { return nil }

// SessionStart is a pure observer.
func (*BrokerHookHandlers) SessionStart(_ context.Context, _ json.RawMessage) error { return nil }

// UserPromptSubmit currently allows everything. v0.1 has no user-prompt
// authz; revisit when the spec adds one.
func (*BrokerHookHandlers) UserPromptSubmit(_ context.Context, _ json.RawMessage) (cc.HookResponse, error) {
	return cc.HookResponse{Decision: "allow"}, nil
}

// Stop is a pure observer.
func (*BrokerHookHandlers) Stop(_ context.Context, _ json.RawMessage) error { return nil }

// summarizeToolInput produces a short human-readable string for the UI
// prompt. We do NOT try to be clever here — the UI gets the full raw
// json in toolInput and can format however it wants. This is a best-
// effort fallback for narrow displays.
func summarizeToolInput(toolName string, input json.RawMessage) string {
	const maxLen = 120
	if len(input) == 0 {
		return toolName
	}
	// Try to extract a few well-known fields for nicer summaries.
	var m map[string]any
	if err := json.Unmarshal(input, &m); err == nil {
		for _, key := range []string{"command", "cmd", "file_path", "path", "url"} {
			if v, ok := m[key]; ok {
				s := fmt.Sprintf("%s: %v", toolName, v)
				if len(s) > maxLen {
					return s[:maxLen-1] + "…"
				}
				return s
			}
		}
	}
	// Fallback: raw json prefix.
	s := fmt.Sprintf("%s %s", toolName, string(input))
	if len(s) > maxLen {
		return s[:maxLen-1] + "…"
	}
	return s
}
