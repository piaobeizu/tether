package daemon_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
	"github.com/piaobeizu/tether/internal/transport/wt"
)

// bootDaemonWithWT spins up a real daemon.Run on a pre-bound :0 UDP
// socket, captures the emitter handle via OnEmitterReady, and returns
// (url, emitter, cancel). Caller must call cancel + wait for done
// before the test exits.
func bootDaemonWithWT(t *testing.T, cfg daemon.Config) (string, *agent.EnvelopeEmitter, func()) {
	t.Helper()

	tmp := t.TempDir()
	if cfg.ProjectsDir == "" {
		cfg.ProjectsDir = filepath.Join(tmp, "projects")
		if err := os.MkdirAll(cfg.ProjectsDir, 0o700); err != nil {
			t.Fatalf("setup projects: %v", err)
		}
	}
	if cfg.AttachSocketPath == "" {
		cfg.AttachSocketPath = filepath.Join(tmp, "attach.sock")
	}
	if cfg.LockAuditLogPath == "" {
		cfg.LockAuditLogPath = filepath.Join(tmp, "lock.log")
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	port := udp.LocalAddr().(*net.UDPAddr).Port
	cfg.WTListener = udp

	emitterCh := make(chan *agent.EnvelopeEmitter, 1)
	prev := cfg.OnEmitterReady
	cfg.OnEmitterReady = func(em *agent.EnvelopeEmitter) {
		if prev != nil {
			prev(em)
		}
		select {
		case emitterCh <- em:
		default:
		}
	}

	ctx, cancelCtx := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	var em *agent.EnvelopeEmitter
	select {
	case em = <-emitterCh:
	case <-time.After(2 * time.Second):
		cancelCtx()
		<-done
		t.Fatal("daemon did not deliver emitter handle within 2s")
	}

	// Give the supervisor a beat to start the wt subsystem accept loop.
	time.Sleep(100 * time.Millisecond)

	cancel := func() {
		cancelCtx()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Run = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("daemon.Run did not exit within 3s of ctx cancel")
		}
	}

	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, wt.EndpointPath)
	return url, em, cancel
}

// dialDaemonWT mirrors wt.Dial with a test-only TLS config that skips
// cert verification (the daemon's auto-generated dev cert isn't
// reachable from outside the wt package).
func dialDaemonWT(t *testing.T, ctx context.Context, url string) *wt.Client {
	t.Helper()
	cli, err := wt.Dial(ctx, wt.ClientConfig{
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
	return cli
}

// TestDaemonWT_EnvelopeFlow_E2E — slice #3 wire-up smoke. Boot daemon
// with --wt-addr=:0 (via WTListener), inject 3 envelopes for a sid,
// connect a Go-side wt.Client, send the v0.1 sid header, accept the
// events stream, decrypt 3 envelopes, assert kinds match.
func TestDaemonWT_EnvelopeFlow_E2E(t *testing.T) {
	t.Parallel()

	const sid = "test-sid-e2e"

	url, em, cancel := bootDaemonWithWT(t, daemon.Config{})
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dialCancel()

	cli := dialDaemonWT(t, dialCtx, url)
	defer cli.Close()

	// 1. Open control + send the v0.1 sid header.
	ctrl, err := cli.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	if err := wt.WriteSessionIDHeader(ctrl, sid); err != nil {
		t.Fatalf("WriteSessionIDHeader: %v", err)
	}

	// 2. Accept events stream (server opens after seeing header +
	// subscribing to the emitter).
	events, err := cli.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}

	// 3. Now that the server is subscribed, inject 3 envelopes.
	// Spec: Inject is best-effort — we wait briefly for the subscription
	// to be live before injecting (AcceptEvents returning means the
	// server has opened the stream, which it does AFTER Subscribe).
	want := []agent.LocalEnvelope{
		{Kind: "output.agent-event", SessionID: sid, ProviderType: "claude-code", PlaintextMetadata: map[string]any{"i": 1}},
		{Kind: "output.hook-event", SessionID: sid, ProviderType: "claude-code", PlaintextMetadata: map[string]any{"i": 2}},
		{Kind: "output.agent-event", SessionID: sid, ProviderType: "claude-code", PlaintextMetadata: map[string]any{"i": 3}},
	}
	for _, env := range want {
		delivered, err := em.Inject(env)
		if err != nil {
			t.Fatalf("Inject: %v", err)
		}
		if delivered != 1 {
			t.Fatalf("Inject delivered=%d, want 1 (no subscriber?)", delivered)
		}
	}

	// 4. Read + decrypt 3 frames.
	er, err := wt.NewEnvelopeFrameReader(events, wt.DevSharedKey[:], sid)
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader: %v", err)
	}
	for i := 0; i < len(want); i++ {
		readCtx, rcancel := context.WithTimeout(dialCtx, 3*time.Second)
		type result struct {
			env *wt.WireEnvelope
			pt  []byte
			err error
		}
		ch := make(chan result, 1)
		go func() {
			env, pt, err := er.Next()
			ch <- result{env, pt, err}
		}()
		var r result
		select {
		case r = <-ch:
		case <-readCtx.Done():
			rcancel()
			t.Fatalf("frame[%d]: read timeout", i)
		}
		rcancel()
		if r.err != nil {
			t.Fatalf("frame[%d] Next: %v", i, r.err)
		}
		if r.env.Kind != want[i].Kind {
			t.Errorf("frame[%d] kind=%q want %q", i, r.env.Kind, want[i].Kind)
		}
		if r.env.FromDeviceID != "daemon-default" {
			t.Errorf("frame[%d] fromDeviceId=%q want daemon-default", i, r.env.FromDeviceID)
		}
		if r.env.ToDeviceID == "" {
			t.Errorf("frame[%d] toDeviceId empty", i)
		}
		var inner agent.LocalEnvelope
		if err := json.Unmarshal(r.pt, &inner); err != nil {
			t.Fatalf("frame[%d] inner unmarshal: %v", i, err)
		}
		if inner.SessionID != sid {
			t.Errorf("frame[%d] inner.sessionId=%q want %q", i, inner.SessionID, sid)
		}
		if inner.Kind != want[i].Kind {
			t.Errorf("frame[%d] inner.Kind=%q want %q", i, inner.Kind, want[i].Kind)
		}
	}
}

// TestDaemonWT_NoSession_NoFlow — WTListenAddr empty + no listener →
// daemon does not open the WT path. We assert by looking for the
// "wt listener starting" log line; absence proves the subsystem
// didn't run.
func TestDaemonWT_NoSession_NoFlow(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var stderrBuf syncBuf
	cfg := daemon.Config{
		Verbose:          true,
		Stderr:           &stderrBuf,
		ProjectsDir:      projectsDir,
		AttachSocketPath: filepath.Join(tmp, "attach.sock"),
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		// WTListenAddr + WTListener intentionally zero.
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("daemon.Run did not exit within 3s of ctx cancel")
	}

	logs := stderrBuf.String()
	if strings.Contains(logs, "wt listener starting") {
		t.Errorf("expected NO wt listener log; got:\n%s", logs)
	}
}

// TestDaemonWT_KeySource_Override — custom WTKeySource returns a
// different key from DevSharedKey. Client decrypts MUST fail when it
// uses DevSharedKey, and succeed when it uses the matching key.
func TestDaemonWT_KeySource_Override(t *testing.T) {
	t.Parallel()

	const sid = "test-sid-keysource"

	// 32 bytes, deliberately != DevSharedKey.
	customKey := make([]byte, wt.SharedKeySize)
	for i := range customKey {
		customKey[i] = byte(0xA5 ^ i)
	}

	var keyCalls atomic.Int32
	url, em, cancel := bootDaemonWithWT(t, daemon.Config{
		WTKeySource: func(s string) ([]byte, error) {
			keyCalls.Add(1)
			if s != sid {
				return nil, fmt.Errorf("unexpected sid: %s", s)
			}
			out := make([]byte, len(customKey))
			copy(out, customKey)
			return out, nil
		},
	})
	defer cancel()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer dialCancel()

	cli := dialDaemonWT(t, dialCtx, url)
	defer cli.Close()

	ctrl, err := cli.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl: %v", err)
	}
	if err := wt.WriteSessionIDHeader(ctrl, sid); err != nil {
		t.Fatalf("WriteSessionIDHeader: %v", err)
	}
	events, err := cli.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents: %v", err)
	}

	delivered, err := em.Inject(agent.LocalEnvelope{
		Kind:      "output.agent-event",
		SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("delivered=%d", delivered)
	}

	// Read one frame with the WRONG key — must fail.
	wrongReader, err := wt.NewEnvelopeFrameReader(events, wt.DevSharedKey[:], sid)
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader (wrong key): %v", err)
	}
	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, _, err := wrongReader.Next()
		ch <- result{err}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatalf("expected open failure with wrong key, got nil")
		}
		if !errors.Is(r.err, wt.ErrWireEnvelope) {
			t.Errorf("expected ErrWireEnvelope, got %v", r.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frame read with wrong key did not fail within 3s")
	}

	// Now try a fresh session with the matching key — must succeed.
	cli2 := dialDaemonWT(t, dialCtx, url)
	defer cli2.Close()
	ctrl2, err := cli2.OpenControl(dialCtx)
	if err != nil {
		t.Fatalf("OpenControl 2: %v", err)
	}
	if err := wt.WriteSessionIDHeader(ctrl2, sid); err != nil {
		t.Fatalf("WriteSessionIDHeader 2: %v", err)
	}
	events2, err := cli2.AcceptEvents(dialCtx)
	if err != nil {
		t.Fatalf("AcceptEvents 2: %v", err)
	}

	// Wait for the second subscription to be live, then inject.
	// Since the second client subscribes after the first is still
	// subscribed (and has stuck/unread on the wrong-key path), Inject
	// will fan-out to BOTH. We just need the second one to receive at
	// least one good frame.
	for retry := 0; retry < 5; retry++ {
		delivered, err = em.Inject(agent.LocalEnvelope{
			Kind:      "output.agent-event",
			SessionID: sid,
			PlaintextMetadata: map[string]any{
				"retry": retry,
			},
		})
		if err != nil {
			t.Fatalf("Inject 2: %v", err)
		}
		if delivered >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	rightReader, err := wt.NewEnvelopeFrameReader(events2, customKey, sid)
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader (right key): %v", err)
	}
	gotCh := make(chan error, 1)
	go func() {
		_, _, err := rightReader.Next()
		gotCh <- err
	}()
	select {
	case err := <-gotCh:
		if err != nil {
			t.Fatalf("Next with matching key: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("frame read with matching key did not succeed within 3s")
	}

	if keyCalls.Load() < 2 {
		t.Errorf("WTKeySource called %d times, want >= 2", keyCalls.Load())
	}
}

