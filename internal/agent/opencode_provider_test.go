package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestOCSession builds a bare opencodeSession suitable for exercising the
// pure parsing/lifecycle helpers without spawning any real subprocess.
func newTestOCSession() *opencodeSession {
	return &opencodeSession{
		events: make(chan Event, 64),
		sidCh:  make(chan struct{}),
	}
}

// TestOpenCodeReadSSE_TextDelta verifies a message.part.delta text frame yields
// a token-level EventText and publishes the session id.
func TestOpenCodeReadSSE_TextDelta(t *testing.T) {
	s := newTestOCSession()
	body := strings.Join([]string{
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":"Hel"}}}`,
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":"lo"}}}`,
		// non-text field must be ignored
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"reasoning","delta":"x"}}}`,
		"",
	}, "\n")

	var got []Event
	s.readSSE(context.Background(), strings.NewReader(body), func(ev Event) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected 2 text events, got %d: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.Kind != EventText {
			t.Errorf("Kind = %q, want %q", ev.Kind, EventText)
		}
	}
	if got[0].Text != "Hel" || got[1].Text != "lo" {
		t.Errorf("deltas = %q,%q; want Hel,lo", got[0].Text, got[1].Text)
	}
	if s.SessionID() != "ses_1" {
		t.Errorf("SessionID = %q, want ses_1", s.SessionID())
	}
}

// TestOpenCodeReadSSE_CreatedAndError covers session.created -> EventInit and
// session.error -> EventError.
func TestOpenCodeReadSSE_CreatedAndError(t *testing.T) {
	s := newTestOCSession()
	body := strings.Join([]string{
		`data: {"payload":{"type":"session.created","properties":{"sessionID":"ses_9"}}}`,
		`data: {"payload":{"type":"session.error","properties":{"error":{"message":"boom"}}}}`,
		// unrecognized type is ignored
		`data: {"payload":{"type":"session.idle","properties":{}}}`,
		// non-SSE line ignored
		`: keep-alive`,
		"",
	}, "\n")

	var got []Event
	s.readSSE(context.Background(), strings.NewReader(body), func(ev Event) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected 2 events (init+error), got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventInit || got[0].SessionID != "ses_9" {
		t.Errorf("event0 = %+v, want EventInit/ses_9", got[0])
	}
	if got[1].Kind != EventError || got[1].Err == nil || got[1].Err.Error() != "boom" {
		t.Errorf("event1 = %+v, want EventError/boom", got[1])
	}
}

// TestOpenCodeInterrupt_IdleNoOp — Interrupt() on a session with no in-flight
// prompt must be a no-op (must not touch the nil serve / panic).
func TestOpenCodeInterrupt_IdleNoOp(t *testing.T) {
	s := newTestOCSession() // busy == false, serve == nil
	if err := s.Interrupt(); err != nil {
		t.Fatalf("idle Interrupt() = %v, want nil", err)
	}
	s.mu.RLock()
	dormant := s.dormant
	s.mu.RUnlock()
	if dormant {
		t.Error("idle Interrupt() must not hibernate the serve (dormant=true)")
	}
}

// TestOpenCodeCloseEvents_Idempotent — closeEvents must be safe to call twice.
func TestOpenCodeCloseEvents_Idempotent(t *testing.T) {
	s := newTestOCSession()
	s.closeEvents()
	s.closeEvents() // must not panic on double close
	if !s.closed {
		t.Error("closed flag not set")
	}
	// emit after close is dropped, not a panic.
	s.emit(Event{Kind: EventText, Text: "late"})
}

// TestOpenCodeInterrupt_Integration exercises the real spawn -> prompt ->
// interrupt -> resume flow against an installed `opencode` binary. It is gated
// behind TETHER_OPENCODE_IT (and skipped if opencode is absent) because it
// spawns real subprocesses and makes a model call. Run locally with:
//
//	TETHER_OPENCODE_IT=1 GOWORK=off go test ./internal/agent/ -run Integration -v
//
// The model is pinned to a free opencode model via an opencode.json written
// into the run workdir; override with TETHER_OPENCODE_MODEL.
func TestOpenCodeInterrupt_Integration(t *testing.T) {
	if os.Getenv("TETHER_OPENCODE_IT") == "" {
		t.Skip("set TETHER_OPENCODE_IT=1 to run the opencode integration test")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not found in PATH")
	}
	model := os.Getenv("TETHER_OPENCODE_MODEL")
	if model == "" {
		model = "opencode/deepseek-v4-flash-free"
	}

	// Pin the model via a project config in the workdir so we don't burn paid
	// credits (the provider doesn't pass --model; opencode reads cwd config).
	work := t.TempDir()
	cfg := `{"$schema":"https://opencode.ai/config.json","model":"` + model + `"}`
	if err := os.WriteFile(filepath.Join(work, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: work})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	var counter chanCounter
	go func() {
		for ev := range sess.Events() {
			if ev.Kind == EventText {
				counter.incr()
			}
		}
	}()

	// First prompt: a long generation, so it's still running when we interrupt.
	const longPrompt = "Write an extremely long, exhaustive 1500-word essay on the full history of computing. Use many paragraphs and be thorough."
	if err := sess.SendPrompt(ctx, longPrompt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !waitFor(30*time.Second, func() bool { return counter.get() > 2 }) {
		t.Fatalf("no token deltas flowed within 30s (n=%d) — check model/creds", counter.get())
	}

	// Interrupt: must hibernate (kill) the serve and stop the delta flow.
	if err := sess.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	oc.mu.RLock()
	dormant, exitDone := oc.dormant, oc.serveExitDone
	oc.mu.RUnlock()
	if !dormant {
		t.Error("after Interrupt() dormant should be true")
	}
	select { // Interrupt waits on exitDone before returning, so it's closed.
	case <-exitDone:
	default:
		t.Error("serve exitDone not closed after Interrupt()")
	}
	// Let any in-flight SSE settle, then confirm the flow has actually stopped
	// (this is the whole point: killing the serve stops generation).
	time.Sleep(4 * time.Second)
	afterGrace := counter.get()
	time.Sleep(3 * time.Second)
	if delta := counter.get() - afterGrace; delta > 0 {
		t.Errorf("deltas kept flowing after interrupt+grace (+%d) — serve not stopped", delta)
	}

	// Resume: a fresh prompt must relaunch the serve and produce output again.
	resumeBase := counter.get()
	if err := sess.SendPrompt(ctx, "Reply with the single word: resumed."); err != nil {
		t.Fatalf("resume send: %v", err)
	}
	if !waitFor(40*time.Second, func() bool { return counter.get() > resumeBase }) {
		t.Fatalf("resume produced no new output within 40s (serve relaunch failed)")
	}
	oc.mu.RLock()
	stillDormant := oc.dormant
	oc.mu.RUnlock()
	if stillDormant {
		t.Error("after resume SendPrompt dormant should be cleared")
	}
}

// chanCounter is a tiny concurrency-safe counter for the integration test.
type chanCounter struct {
	mu sync.Mutex
	n  int
}

func (c *chanCounter) incr() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *chanCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}
