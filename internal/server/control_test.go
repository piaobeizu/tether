package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

func TestRespondToControl_Ping(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFramePing, TS: 1234567890}
	resp, ok := RespondToControl(f)
	if !ok {
		t.Fatal("expected ok=true for ping frame")
	}
	if resp == nil {
		t.Fatal("expected non-nil ControlFrame for ping")
	}
	if resp.Kind != wire.ControlPong {
		t.Fatalf("resp.Kind = %q, want %q", resp.Kind, wire.ControlPong)
	}
	if resp.TS != f.TS {
		t.Fatalf("resp.TS = %d, want %d (echoed)", resp.TS, f.TS)
	}
}

func TestRespondToControl_PingZeroTS(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFramePing, TS: 0}
	resp, ok := RespondToControl(f)
	if !ok {
		t.Fatal("expected ok=true for ping frame with ts=0")
	}
	if resp.TS != 0 {
		t.Fatalf("resp.TS = %d, want 0", resp.TS)
	}
}

func TestRespondToControl_UnknownKind(t *testing.T) {
	f := wire.ClientFrame{Kind: wire.ClientFrameAction, Action: "approve", BlockID: "b1"}
	resp, ok := RespondToControl(f)
	if ok {
		t.Fatal("expected ok=false for non-ping frame")
	}
	if resp != nil {
		t.Fatalf("expected nil ControlFrame, got %+v", resp)
	}
}

func TestRespondToControl_EmptyKind(t *testing.T) {
	f := wire.ClientFrame{}
	resp, ok := RespondToControl(f)
	if ok || resp != nil {
		t.Fatalf("expected (nil, false) for empty frame, got (%+v, %v)", resp, ok)
	}
}

// ─── handleActionFrame (tether#8 T8) ────────────────────────────────────────
//
// fakeActionSession/fakeActionProvider are a minimal agent.Session /
// agent.AgentProvider pair (package-local, distinct from internal/session's
// own unexported fakes) so handleActionFrame's routing can be exercised
// end-to-end through a real *session.Registry without a live cc subprocess.

type fakeActionSession struct {
	sid    string
	events chan agent.Event

	mu             sync.Mutex
	prompts        []string
	interruptCalls int
}

func (f *fakeActionSession) SessionID() string { return f.sid }
func (f *fakeActionSession) SendPrompt(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	return nil
}
func (f *fakeActionSession) Events() <-chan agent.Event { return f.events }
func (f *fakeActionSession) Interrupt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	return nil
}
func (f *fakeActionSession) Close() error { return nil }

func (f *fakeActionSession) Prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// InterruptCalls returns how many times Interrupt() has been called so far.
func (f *fakeActionSession) InterruptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interruptCalls
}

type fakeActionProvider struct{ sess *fakeActionSession }

func (p *fakeActionProvider) Name() string { return "fake" }
func (p *fakeActionProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	return p.sess, nil
}

// waitForLive polls until sid is registered in reg (GetOrSpawnEntry re-keys
// asynchronously once the session's real id resolves — see its doc
// comment), bounded so a genuine bug fails fast instead of hanging.
func waitForLive(t *testing.T, reg *session.Registry, sid string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if reg.IsLive(sid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %q never registered under its real sid", sid)
}

// TestHandleActionFrame_ApproveRoutesToSession — an action{approve,
// sessionId} frame is delivered to the target session's agent (via
// Registry.DeliverAction / SendPrompt), wrapped in the __tether_action__
// marker (docs/wire/fenced-contract.md §5).
func TestHandleActionFrame_ApproveRoutesToSession(t *testing.T) {
	fs := &fakeActionSession{sid: "sid-approve-1", events: make(chan agent.Event, 4)}
	reg := session.NewRegistry(&fakeActionProvider{sess: fs})
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForLive(t, reg, "sid-approve-1")

	handleActionFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameAction,
		SessionID: "sid-approve-1",
		Action:    "approve",
		BlockID:   "s-0",
		Skill:     "planner",
	})

	prompts := fs.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("len(prompts) = %d, want 1: %+v", len(prompts), prompts)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(prompts[0]), &got); err != nil {
		t.Fatalf("delivered payload not valid JSON: %v (%q)", err, prompts[0])
	}
	inner, ok := got["__tether_action__"].(map[string]any)
	if !ok {
		t.Fatalf("delivered payload missing __tether_action__ marker: %q", prompts[0])
	}
	if inner["action"] != "approve" || inner["blockId"] != "s-0" || inner["skill"] != "planner" {
		t.Errorf("__tether_action__ = %+v, want {action:approve blockId:s-0 skill:planner}", inner)
	}
}

// TestHandleActionFrame_UnknownSessionDropped — an action frame naming a
// session the registry has never heard of must be dropped, not panic, and
// must not deliver anything anywhere.
func TestHandleActionFrame_UnknownSessionDropped(t *testing.T) {
	fs := &fakeActionSession{sid: "sid-approve-2", events: make(chan agent.Event, 4)}
	reg := session.NewRegistry(&fakeActionProvider{sess: fs})

	handleActionFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameAction,
		SessionID: "does-not-exist",
		Action:    "approve",
		BlockID:   "s-0",
		Skill:     "planner",
	})

	if got := fs.Prompts(); len(got) != 0 {
		t.Fatalf("Prompts() = %v, want none delivered for an unknown session", got)
	}
}

// TestHandleActionFrame_PauseRoutesToInterrupt — (tether#8 T9) a pause
// action frame must reach the session's agent.Session.Interrupt() via
// Registry.InterruptSession, and must NOT go through the
// SendPrompt/__tether_action__ path (that's approve's delivery mechanism,
// not pause's — pause is a transport-level interrupt).
func TestHandleActionFrame_PauseRoutesToInterrupt(t *testing.T) {
	fs := &fakeActionSession{sid: "sid-pause", events: make(chan agent.Event, 4)}
	reg := session.NewRegistry(&fakeActionProvider{sess: fs})
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForLive(t, reg, "sid-pause")

	handleActionFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameAction,
		SessionID: "sid-pause",
		Action:    "pause",
		BlockID:   "s-0",
	})

	if got := fs.InterruptCalls(); got != 1 {
		t.Fatalf("InterruptCalls() = %d, want 1", got)
	}
	if got := fs.Prompts(); len(got) != 0 {
		t.Fatalf("Prompts() = %v, want none — pause must not call SendPrompt", got)
	}
}

// TestHandleActionFrame_PauseUnknownSessionDropped — an unknown/already-
// ended SessionID on a pause frame is the same expected race as approve's:
// InterruptSession's error is logged and dropped, never a crash.
func TestHandleActionFrame_PauseUnknownSessionDropped(t *testing.T) {
	fs := &fakeActionSession{sid: "sid-pause-2", events: make(chan agent.Event, 4)}
	reg := session.NewRegistry(&fakeActionProvider{sess: fs})

	handleActionFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameAction,
		SessionID: "does-not-exist",
		Action:    "pause",
		BlockID:   "s-0",
	})

	if got := fs.InterruptCalls(); got != 0 {
		t.Fatalf("InterruptCalls() = %d, want 0 — unknown session must not be reached", got)
	}
}

// TestHandleActionFrame_RollbackIgnored — rollback has no aihub primitive
// and stays permanently unwired; the frame must be ignored, not panic.
func TestHandleActionFrame_RollbackIgnored(t *testing.T) {
	fs := &fakeActionSession{sid: "sid-rollback", events: make(chan agent.Event, 4)}
	reg := session.NewRegistry(&fakeActionProvider{sess: fs})
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForLive(t, reg, "sid-rollback")

	handleActionFrame(reg, wire.ClientFrame{
		Kind:      wire.ClientFrameAction,
		SessionID: "sid-rollback",
		Action:    "rollback",
		BlockID:   "s-0",
	})

	if got := fs.Prompts(); len(got) != 0 {
		t.Fatalf("Prompts() = %v, want none — rollback is not wired", got)
	}
}
