// Package agent — tool-authorization broker (PreToolUse → UI prompt → decision).
//
// Wire shape
// ----------
// Two new LocalEnvelope kinds carry the round-trip:
//
//   • KindAuthToolRequest  ("auth.tool-request")  daemon → UI
//   • KindAuthToolDecision ("auth.tool-decision") UI    → daemon (input frame)
//
// DO NOT rename without updating tether-app/src/components/AuthPrompt.tsx
// and tether-app/src/transport/auth.ts — the field names below are pinned
// across both repos. Wire shape is byte-identical on Go and TS sides.
//
// Request envelope (daemon → UI):
//   LocalEnvelope{
//     Kind:      "auth.tool-request",
//     SessionID: "<cc session id>",
//     PlaintextMetadata: {
//       "requestId": "<ULID-ish unique id, daemon-issued>",
//       "toolName":  "Bash",
//       "toolInput": <raw json passthrough from cc PreToolUse>,
//       "summary":   "<short human-readable args summary, ≤120 chars>"
//     }
//   }
//
// Decision frame (UI → daemon, length-prefixed JSON on the rw input
// direction of the attach socket):
//   {
//     "type":       "auth.tool-decision",
//     "requestId":  "<echoes request>",
//     "decision":   "allow-once" | "allow-always" | "deny-once" | "deny-always"
//   }
//
// "always" is per-(sessionId, toolName) and lives in-process only — restart
// the daemon and the user is asked again. Persistence is a deliberate
// follow-up: v0.1 keeps the cache in RAM so misclicks don't survive a
// daemon crash.
//
// Timeout: Ask() blocks up to 60s by default (AuthBroker.Timeout).
// On timeout the broker logs and returns deny — fail-closed.

package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// LocalEnvelope kind tags reserved for tool-authorization. Keeping these
// as exported constants so the attach socket / UI bridge can dispatch
// without stringly-typed magic.
//
// DO NOT rename without updating tether-app/src/components/AuthPrompt.tsx
// (frame.body.kind switch) — these strings are the cross-repo contract.
const (
	KindAuthToolRequest  = "auth.tool-request"
	KindAuthToolDecision = "auth.tool-decision"
)

// AuthDecision is the four-valued enum the UI returns. The strings are
// pinned to the wire — if you rename, update the TS side in lock-step.
type AuthDecision string

const (
	AuthDecisionAllowOnce   AuthDecision = "allow-once"
	AuthDecisionAllowAlways AuthDecision = "allow-always"
	AuthDecisionDenyOnce    AuthDecision = "deny-once"
	AuthDecisionDenyAlways  AuthDecision = "deny-always"
)

// IsAllow returns true for both single-shot and remembered allow decisions.
func (d AuthDecision) IsAllow() bool {
	return d == AuthDecisionAllowOnce || d == AuthDecisionAllowAlways
}

// IsAlways returns true for sticky decisions ("always" prefix). Used by
// the broker to populate the remember-cache.
func (d AuthDecision) IsAlways() bool {
	return d == AuthDecisionAllowAlways || d == AuthDecisionDenyAlways
}

// AuthDecisionFrame is the wire shape the UI sends back over the rw
// input direction of the attach socket. Field tags are pinned — DO NOT
// rename without updating tether-app/src/transport/auth.ts.
type AuthDecisionFrame struct {
	Type      string       `json:"type"`
	RequestID string       `json:"requestId"`
	Decision  AuthDecision `json:"decision"`
}

// DefaultAuthTimeout caps the wait for a decision. Chosen at 60s as the
// upper bound for "user is at the keyboard but not actively watching" —
// long enough that a context-switch doesn't cause spurious denials,
// short enough that a forgotten prompt doesn't pin the cc tool call
// indefinitely. Tunable via AuthBroker.Timeout.
const DefaultAuthTimeout = 60 * time.Second

// AuthBroker mediates PreToolUse → UI prompt → decision. One broker
// per daemon instance.
//
// Concurrency: all methods are safe for concurrent invocation. Ask()
// from multiple cc tool calls in parallel each get their own requestId
// and channel. SubmitDecision() routes by requestId.
type AuthBroker struct {
	// Emitter is how the broker pushes the request envelope to attached
	// UI clients. Required.
	Emitter *EnvelopeEmitter

	// Timeout overrides DefaultAuthTimeout. Zero = use default.
	Timeout time.Duration

	// OnDecision is called once per Ask() resolution (allow/deny/timeout)
	// for daemon-side audit logging. Optional.
	OnDecision func(sid, toolName string, decision AuthDecision, fromCache bool)

	mu       sync.Mutex
	pending  map[string]*pendingAuth         // requestId → channel
	remember map[rememberKey]AuthDecision    // (sid, toolName) → "always" decision
	closed   bool
}

type pendingAuth struct {
	sid      string
	toolName string
	ch       chan AuthDecision
}

type rememberKey struct {
	sid  string
	tool string
}

// ErrAuthBrokerClosed is returned by Ask after Close has run.
var ErrAuthBrokerClosed = errors.New("agent: auth broker closed")

// NewAuthBroker constructs a broker. Emitter is required.
func NewAuthBroker(em *EnvelopeEmitter) (*AuthBroker, error) {
	if em == nil {
		return nil, errors.New("agent: NewAuthBroker requires an Emitter")
	}
	return &AuthBroker{
		Emitter:  em,
		pending:  make(map[string]*pendingAuth),
		remember: make(map[rememberKey]AuthDecision),
	}, nil
}

// Ask emits a request envelope to UI clients and blocks for a decision
// up to the configured timeout. Returns the decision; on timeout returns
// AuthDecisionDenyOnce + a non-nil error so the caller sees the wall-
// clock failure (fail-closed). ctx cancellation also returns deny+ctx.Err.
//
// toolInput is forwarded verbatim as plaintext metadata; summary is the
// short string surfaced in the UI ("≤120 chars" is a soft target, the
// broker doesn't truncate).
func (b *AuthBroker) Ask(ctx context.Context, sid, toolName string, toolInput json.RawMessage, summary string) (AuthDecision, error) {
	if sid == "" || toolName == "" {
		return AuthDecisionDenyOnce, errors.New("agent: AuthBroker.Ask requires sid + toolName")
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return AuthDecisionDenyOnce, ErrAuthBrokerClosed
	}
	// Cache hit — short-circuit before bothering the UI.
	if cached, ok := b.remember[rememberKey{sid, toolName}]; ok {
		b.mu.Unlock()
		if b.OnDecision != nil {
			b.OnDecision(sid, toolName, cached, true)
		}
		return cached, nil
	}
	rid := newRequestID()
	pa := &pendingAuth{sid: sid, toolName: toolName, ch: make(chan AuthDecision, 1)}
	b.pending[rid] = pa
	b.mu.Unlock()

	// Always clean up the pending entry on exit.
	defer func() {
		b.mu.Lock()
		delete(b.pending, rid)
		b.mu.Unlock()
	}()

	env := LocalEnvelope{
		Kind:      KindAuthToolRequest,
		SessionID: sid,
		PlaintextMetadata: map[string]any{
			"requestId": rid,
			"toolName":  toolName,
			"toolInput": toolInput,
			"summary":   summary,
		},
	}
	if err := b.Emitter.Inject(env); err != nil {
		return AuthDecisionDenyOnce, fmt.Errorf("agent: broker inject: %w", err)
	}

	timeout := b.Timeout
	if timeout <= 0 {
		timeout = DefaultAuthTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case d := <-pa.ch:
		if d.IsAlways() {
			b.mu.Lock()
			b.remember[rememberKey{sid, toolName}] = d
			b.mu.Unlock()
		}
		if b.OnDecision != nil {
			b.OnDecision(sid, toolName, d, false)
		}
		return d, nil
	case <-timer.C:
		if b.OnDecision != nil {
			b.OnDecision(sid, toolName, AuthDecisionDenyOnce, false)
		}
		return AuthDecisionDenyOnce, fmt.Errorf("agent: auth decision timeout after %s", timeout)
	case <-ctx.Done():
		return AuthDecisionDenyOnce, ctx.Err()
	}
}

// SubmitDecision is invoked by the attach socket reader when a decision
// frame arrives from the UI. Returns nil on successful routing; non-nil
// if the requestId is unknown (already timed out / never issued — the
// caller should log + drop, not panic).
func (b *AuthBroker) SubmitDecision(frame AuthDecisionFrame) error {
	if frame.RequestID == "" {
		return errors.New("agent: empty requestId in decision frame")
	}
	switch frame.Decision {
	case AuthDecisionAllowOnce, AuthDecisionAllowAlways,
		AuthDecisionDenyOnce, AuthDecisionDenyAlways:
		// ok
	default:
		return fmt.Errorf("agent: unknown decision %q", frame.Decision)
	}
	b.mu.Lock()
	pa, ok := b.pending[frame.RequestID]
	b.mu.Unlock()
	if !ok {
		return fmt.Errorf("agent: no pending auth for requestId %q (timed out?)", frame.RequestID)
	}
	select {
	case pa.ch <- frame.Decision:
		return nil
	default:
		// Channel was already filled (duplicate decision). Idempotent —
		// drop the second.
		return nil
	}
}

// ForgetRemembered drops a sticky "always" entry (if any) for
// (sid, toolName). Useful for a future "revoke" UX; v0.1 has no caller
// but it's the natural inverse and keeps the surface symmetric.
func (b *AuthBroker) ForgetRemembered(sid, toolName string) {
	b.mu.Lock()
	delete(b.remember, rememberKey{sid, toolName})
	b.mu.Unlock()
}

// Close fails any pending Ask() with ErrAuthBrokerClosed. Idempotent.
func (b *AuthBroker) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	pendings := b.pending
	b.pending = nil
	b.mu.Unlock()
	for _, pa := range pendings {
		// Non-blocking send — Ask() will fall through to the timer/ctx
		// path if the channel is already full. We use deny-once as the
		// shutdown signal.
		select {
		case pa.ch <- AuthDecisionDenyOnce:
		default:
		}
	}
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is essentially impossible on linux/darwin/win
		// but if it does happen, fall back to a wall-clock id so Ask() can
		// still proceed (the requestId only needs uniqueness within a
		// daemon run, not unforgeability).
		return fmt.Sprintf("auth-%d", time.Now().UnixNano())
	}
	return "auth-" + hex.EncodeToString(b[:])
}
