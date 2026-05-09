package daemon_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
	"github.com/piaobeizu/tether/internal/transport/wt"
)

// TestWTIntegration_AuthDecisionRoutes pins the C1 fix: the daemon's
// WT subsystem must route auth.tool-decision JSON lines that arrive on
// the control stream (after the SessionIDHeader) into AuthBroker.
//
// Repro of the bug this fix closes: pre-fix, wtsubsystem.go::
// handleSession peeked the first control-stream line for pair.invite vs
// SessionIDHeader and then never read the control stream again. The UI
// would PUT auth.tool-decision over the WT control stream (mirror of
// the UDS path the cc tests already exercise), and the daemon would
// drop it on the floor — broker.Ask() blocked its full timeout, fail-
// closed denying every cc tool call.
//
// Test shape (mirrors hookserver_integration_test.go's
// TestIntegration_HookServerWireup_NoInputSink_AuthDecisionFlows but
// for the WT path):
//
//  1. Boot a daemon with EnableAuthBroker=true + WT enabled. Capture
//     handles to the emitter (so we can verify Inject reaches the WT
//     subscriber) and broker (so we can drive Ask + assert it returns
//     allow once we route the decision).
//  2. Open a Go-side WT client; send the SessionIDHeader + accept the
//     events stream. This brings the WT control-frame dispatcher loop
//     online for the test sid.
//  3. Run broker.Ask(sid, "Bash", ...) in a goroutine. Inject inside
//     Ask emits a tool-request envelope; the WT events stream carries
//     it (we don't need to read it for this test — only the requestId
//     matters, which we extract via a parallel emitter tap).
//  4. Send a {type:"auth.tool-decision", requestId, decision:"allow-once"}
//     JSON line over the WT control stream.
//  5. Assert: broker.Ask returns AllowOnce. Pre-fix, this would deadline
//     out at the test's 5s mark with DenyOnce.
func TestWTIntegration_AuthDecisionRoutes(t *testing.T) {
	t.Parallel()

	const sid = "test-wt-auth-routes"

	tmp := t.TempDir()
	cfg := daemon.Config{
		ProjectsDir:      filepath.Join(tmp, "projects"),
		AttachSocketPath: filepath.Join(tmp, "attach.sock"),
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		HookSettingsDir:  filepath.Join(tmp, "cc-settings"),
		EnableAuthBroker: true,
		// Disable pair audit + registry for this hermetic test — we
		// don't need the per-device key path; legacy DevSharedKey is
		// fine.
		DisablePairAudit:  true,
		DisablePairServer: true,
		PairRegistryRoot:  filepath.Join(tmp, "pair-reg"),
		// Need an InputSink to keep the daemon's settings.json wiring
		// happy (the hookserver path will fire OnHookSettingsReady but
		// we don't use it).
		InputSink: func(_ string, _ []byte) error { return nil },
	}
	if err := os.MkdirAll(cfg.ProjectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	cfg.WTListener = udp

	emitterCh := make(chan *agent.EnvelopeEmitter, 1)
	cfg.OnEmitterReady = func(em *agent.EnvelopeEmitter) {
		select {
		case emitterCh <- em:
		default:
		}
	}
	brokerCh := make(chan *agent.AuthBroker, 1)
	cfg.OnAuthBrokerReady = func(b *agent.AuthBroker) {
		select {
		case brokerCh <- b:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case derr := <-done:
			if derr != nil {
				t.Errorf("daemon.Run = %v", derr)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("daemon did not shut down within 3s")
		}
	}()

	var em *agent.EnvelopeEmitter
	select {
	case em = <-emitterCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnEmitterReady never fired")
	}
	var broker *agent.AuthBroker
	select {
	case broker = <-brokerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnAuthBrokerReady never fired")
	}

	// Give the WT subsystem a beat to start the accept loop.
	time.Sleep(150 * time.Millisecond)

	// Connect the WT client + send SessionIDHeader.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dialCancel()

	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, wt.EndpointPath)
	cli, err := wt.Dial(dialCtx, wt.ClientConfig{
		URL: url,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // test-only: dev-cert chain unreachable
			NextProtos:         []string{"h3"},
			MinVersion:         tls.VersionTLS13,
		},
	})
	if err != nil {
		t.Fatalf("wt.Dial: %v", err)
	}
	defer cli.Close()

	ctrl, err := cli.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	if err := wt.WriteSessionIDHeader(ctrl, sid); err != nil {
		t.Fatalf("WriteSessionIDHeader: %v", err)
	}

	// Accept the events stream — daemon opens it after subscribing,
	// which guarantees the control dispatcher goroutine is running.
	events, err := cli.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}
	defer events.Close()

	// Tap the emitter for the auth.tool-request envelope so we can
	// extract the requestId. The WT subsystem also subscribes — Inject
	// fan-outs to all subscribers — so this tap is non-destructive.
	envCh, teardown, err := em.Subscribe(sid)
	if err != nil {
		t.Fatalf("emitter Subscribe: %v", err)
	}
	defer teardown()

	// Run broker.Ask in a goroutine; the response is what we ultimately
	// assert on.
	type askResult struct {
		decision agent.AuthDecision
		err      error
	}
	askCh := make(chan askResult, 1)
	go func() {
		d, aerr := broker.Ask(ctx, sid, "Bash", json.RawMessage(`{"command":"ls"}`), "list cwd")
		askCh <- askResult{d, aerr}
	}()

	// Wait for the broker's tool-request envelope to land on our tap.
	var requestID string
	select {
	case env := <-envCh:
		if env.Kind != agent.KindAuthToolRequest {
			t.Fatalf("first emitter envelope kind=%q want %q", env.Kind, agent.KindAuthToolRequest)
		}
		ridRaw, ok := env.PlaintextMetadata["requestId"]
		if !ok {
			t.Fatalf("auth.tool-request missing requestId: %+v", env.PlaintextMetadata)
		}
		requestID, _ = ridRaw.(string)
		if requestID == "" {
			t.Fatalf("requestId not a string: %v", ridRaw)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("auth.tool-request envelope never arrived on emitter tap")
	}

	// Send the auth.tool-decision frame over the WT control stream as
	// AppShell does. wt-attach.ts:207-219 newline-terminates the JSON;
	// mirror that.
	frame := agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: requestID,
		Decision:  agent.AuthDecisionAllowOnce,
	}
	frameBytes, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	frameBytes = append(frameBytes, '\n')
	if _, err := ctrl.Write(frameBytes); err != nil {
		t.Fatalf("write decision over WT control: %v", err)
	}

	// The broker.Ask call should now return allow-once; pre-fix it
	// would block its full timeout (60s) because the daemon never read
	// the line off the WT control stream.
	select {
	case r := <-askCh:
		if r.err != nil {
			t.Fatalf("broker.Ask error=%v (decision=%q) — auth.tool-decision was not routed via the WT control stream",
				r.err, r.decision)
		}
		if r.decision != agent.AuthDecisionAllowOnce {
			t.Fatalf("broker.Ask decision=%q want %q",
				r.decision, agent.AuthDecisionAllowOnce)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("broker.Ask did not return within 5s of decision write — C1 (WT control dispatch) is broken")
	}
}
