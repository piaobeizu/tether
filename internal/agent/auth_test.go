package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/lock"
)

// helper: build an emitter rooted at a tmp dir so the watcher init
// doesn't pull in $HOME.
func newTestEmitter(t *testing.T) *agent.EnvelopeEmitter {
	t.Helper()
	dir := t.TempDir()
	em, err := agent.NewEnvelopeEmitter(t.Context(), agent.EmitterConfig{
		ProjectsDir: dir,
	})
	if err != nil {
		t.Fatalf("emitter: %v", err)
	}
	t.Cleanup(func() { _ = em.Close() })
	return em
}

func TestAuthBroker_AllowOnce(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, err := agent.NewAuthBroker(em)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	t.Cleanup(br.Close)

	// Subscribe so Inject has somewhere to land.
	envCh, cancel, err := em.Subscribe("sid-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	// Drive Ask in a goroutine; capture the requestId from the emitted
	// envelope, then submit the decision.
	type result struct {
		d   agent.AuthDecision
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		d, err := br.Ask(t.Context(), "sid-1", "Bash", json.RawMessage(`{"cmd":"ls"}`), "Bash: ls")
		resCh <- result{d, err}
	}()

	var rid string
	select {
	case env := <-envCh:
		if env.Kind != agent.KindAuthToolRequest {
			t.Fatalf("expected kind %q, got %q", agent.KindAuthToolRequest, env.Kind)
		}
		if env.SessionID != "sid-1" {
			t.Fatalf("sid mismatch: %q", env.SessionID)
		}
		got, _ := env.PlaintextMetadata["requestId"].(string)
		if got == "" {
			t.Fatalf("missing requestId in envelope: %+v", env.PlaintextMetadata)
		}
		rid = got
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for request envelope")
	}

	if err := br.SubmitDecision(agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: rid,
		Decision:  agent.AuthDecisionAllowOnce,
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("Ask err: %v", r.err)
		}
		if r.d != agent.AuthDecisionAllowOnce {
			t.Fatalf("decision: %v", r.d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for Ask result")
	}
}

func TestAuthBroker_RememberAlways(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, err := agent.NewAuthBroker(em)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	t.Cleanup(br.Close)

	envCh, cancel, _ := em.Subscribe("sid-2")
	defer cancel()

	// First call: allow-always.
	go func() {
		env := <-envCh
		rid, _ := env.PlaintextMetadata["requestId"].(string)
		_ = br.SubmitDecision(agent.AuthDecisionFrame{
			Type:      agent.KindAuthToolDecision,
			RequestID: rid,
			Decision:  agent.AuthDecisionAllowAlways,
		})
	}()
	d, err := br.Ask(t.Context(), "sid-2", "Read", json.RawMessage(`{"file_path":"/tmp/x"}`), "Read /tmp/x")
	if err != nil || d != agent.AuthDecisionAllowAlways {
		t.Fatalf("first ask: d=%v err=%v", d, err)
	}

	// Second call should hit cache — no envelope, no submit needed.
	d2, err := br.Ask(t.Context(), "sid-2", "Read", json.RawMessage(`{"file_path":"/tmp/y"}`), "")
	if err != nil {
		t.Fatalf("second ask err: %v", err)
	}
	if d2 != agent.AuthDecisionAllowAlways {
		t.Fatalf("second ask decision: %v (cache miss)", d2)
	}

	// Different sid → cache miss. New subscription to sid-3.
	envCh2, cancel2, _ := em.Subscribe("sid-3")
	defer cancel2()
	go func() {
		env := <-envCh2
		rid, _ := env.PlaintextMetadata["requestId"].(string)
		_ = br.SubmitDecision(agent.AuthDecisionFrame{
			Type:      agent.KindAuthToolDecision,
			RequestID: rid,
			Decision:  agent.AuthDecisionDenyOnce,
		})
	}()
	d3, _ := br.Ask(t.Context(), "sid-3", "Read", json.RawMessage(`{}`), "")
	if d3 != agent.AuthDecisionDenyOnce {
		t.Fatalf("sid-3 should not hit sid-2 cache: got %v", d3)
	}

	// ForgetRemembered — re-ask should fire envelope again.
	br.ForgetRemembered("sid-2", "Read")
	got := make(chan agent.AuthDecision, 1)
	go func() {
		d, _ := br.Ask(t.Context(), "sid-2", "Read", json.RawMessage(`{}`), "")
		got <- d
	}()
	select {
	case env := <-envCh:
		rid, _ := env.PlaintextMetadata["requestId"].(string)
		_ = br.SubmitDecision(agent.AuthDecisionFrame{
			Type:      agent.KindAuthToolDecision,
			RequestID: rid,
			Decision:  agent.AuthDecisionDenyOnce,
		})
	case <-time.After(2 * time.Second):
		t.Fatal("ForgetRemembered did not re-ask")
	}
	if d := <-got; d != agent.AuthDecisionDenyOnce {
		t.Fatalf("post-forget decision: %v", d)
	}
}

func TestAuthBroker_Timeout(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, err := agent.NewAuthBroker(em)
	if err != nil {
		t.Fatalf("broker: %v", err)
	}
	br.Timeout = 50 * time.Millisecond
	t.Cleanup(br.Close)

	// Subscribe so Inject succeeds (even though we'll never decide).
	_, cancel, _ := em.Subscribe("sid-t")
	defer cancel()

	d, err := br.Ask(t.Context(), "sid-t", "Bash", json.RawMessage(`{}`), "")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if d != agent.AuthDecisionDenyOnce {
		t.Fatalf("timeout decision: %v (want deny-once)", d)
	}
}

func TestAuthBroker_CtxCancel(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)

	_, cancelSub, _ := em.Subscribe("sid-c")
	defer cancelSub()

	ctx, cancel := context.WithCancel(t.Context())
	resCh := make(chan error, 1)
	go func() {
		_, err := br.Ask(ctx, "sid-c", "Bash", nil, "")
		resCh <- err
	}()
	cancel()
	select {
	case err := <-resCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err: %v (want context.Canceled)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel did not propagate")
	}
}

func TestAuthBroker_UnknownRequestID(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)

	err := br.SubmitDecision(agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: "auth-bogus",
		Decision:  agent.AuthDecisionAllowOnce,
	})
	if err == nil {
		t.Fatal("expected error on unknown requestId")
	}
}

func TestAuthBroker_Validation(t *testing.T) {
	t.Parallel()
	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)

	// Empty sid.
	if _, err := br.Ask(t.Context(), "", "Bash", nil, ""); err == nil {
		t.Fatal("expected error on empty sid")
	}
	// Empty toolName.
	if _, err := br.Ask(t.Context(), "sid", "", nil, ""); err == nil {
		t.Fatal("expected error on empty toolName")
	}
	// Empty requestId in submit.
	if err := br.SubmitDecision(agent.AuthDecisionFrame{Type: agent.KindAuthToolDecision}); err == nil {
		t.Fatal("expected error on empty requestId")
	}
	// Unknown decision.
	if err := br.SubmitDecision(agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: "x",
		Decision:  agent.AuthDecision("bogus"),
	}); err == nil {
		t.Fatal("expected error on unknown decision string")
	}
}

// Integration-flavored: spin up an AttachServer with a broker and feed
// a JSON decision frame through readInputs to verify the broker
// receives it via the input direction of the attach socket.
func TestAttachServer_AuthDecisionRouting(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "attach.sock")

	em := newTestEmitter(t)
	br, _ := agent.NewAuthBroker(em)
	t.Cleanup(br.Close)
	br.Timeout = 2 * time.Second

	srv, err := agent.NewAttachServer(agent.AttachServerConfig{
		SocketPath: sockPath,
		Emitter:    em,
		Lock:       lock.New(),
		// InputSink stays non-nil so rw mode is allowed; record any
		// non-decision bytes so we can assert the JSON frame did NOT
		// fall through to the PTY sink.
		InputSink: func(_ string, data []byte) error {
			t.Errorf("PTY sink got auth-decision bytes: %q", data)
			return nil
		},
		AuthBroker: br,
		SocketPerm: 0o600,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx) }()

	// Connect.
	c, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Subscribe before sending the header so Ask() finds a subscriber.
	_, cancelSub, _ := em.Subscribe("sid-attach")
	defer cancelSub()

	// Header.
	hdr := []byte(`{"sessionId":"sid-attach","mode":"rw","client":{"kind":"terminal","deviceId":"dev-1"}}` + "\n")
	if _, err := c.Write(hdr); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Read ack.
	if _, err := agent.ReadFrame(c); err != nil {
		t.Fatalf("read ack: %v", err)
	}

	// Issue an Ask (spawns a request envelope) and capture rid.
	resCh := make(chan agent.AuthDecision, 1)
	go func() {
		d, _ := br.Ask(t.Context(), "sid-attach", "Bash", json.RawMessage(`{"cmd":"ls"}`), "Bash: ls")
		resCh <- d
	}()

	// Read frames until we see the auth-tool-request envelope. The
	// daemon only fans request envelopes via Inject — here we read
	// them via the attach socket subscriber.
	var requestID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		frame, err := agent.ReadFrame(c)
		if err != nil {
			// timeout — keep polling
			continue
		}
		var env agent.LocalEnvelope
		if err := json.Unmarshal(frame, &env); err == nil {
			if env.Kind == agent.KindAuthToolRequest {
				rid, _ := env.PlaintextMetadata["requestId"].(string)
				requestID = rid
				break
			}
		}
	}
	if requestID == "" {
		t.Fatal("did not see auth.tool-request envelope")
	}

	// Send the decision frame back via the input direction.
	decision := agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: requestID,
		Decision:  agent.AuthDecisionAllowOnce,
	}
	body, _ := json.Marshal(decision)
	if err := agent.WriteInputFrame(c, body); err != nil {
		t.Fatalf("write input frame: %v", err)
	}

	// Ask should resolve.
	select {
	case d := <-resCh:
		if d != agent.AuthDecisionAllowOnce {
			t.Fatalf("decision: %v", d)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ask did not resolve from socket-routed decision")
	}

	cancel()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Error("server did not exit after ctx cancel")
	}
	_ = os.Remove(sockPath)
}
