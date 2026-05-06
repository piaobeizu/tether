package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
	"github.com/piaobeizu/tether/internal/watchdog"
)

// TestRun_StartsAndDrainsOnCancel — minimum smoke test: calling
// Run with the production composition (real envelope emitter +
// attach socket, but rooted at temp dirs) should boot, supervise,
// and exit cleanly when ctx cancels.
func TestRun_StartsAndDrainsOnCancel(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	var stderr bytes.Buffer
	cfg := daemon.Config{
		Verbose:          true,
		Stderr:           &stderr,
		ProjectsDir:      projectsDir,
		AttachSocketPath: attachSocket,
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Let it run a moment so we see "starting" log lines for both
	// subsystems and the attach socket actually binds.
	time.Sleep(150 * time.Millisecond)

	if _, err := os.Stat(attachSocket); err != nil {
		t.Errorf("attach socket %s should exist after daemon boot: %v", attachSocket, err)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon.Run did not exit within 2s of ctx cancel")
	}

	logs := stderr.String()
	for _, want := range []string{"daemon", "client", "starting"} {
		if !strings.Contains(logs, want) {
			t.Errorf("expected logs to mention %q; got:\n%s", want, logs)
		}
	}
}

// TestRun_CustomFactory — a Config-injected factory is honored
// (this is the seam tests + later real implementations use to wire
// the actual daemon/client subsystems).
func TestRun_CustomFactory(t *testing.T) {
	t.Parallel()

	var beats atomic.Int64
	factory := func() watchdog.Subsystem {
		return watchdog.SubsystemFunc{
			N: "test-sub",
			F: func(ctx context.Context, hb func()) error {
				t := time.NewTicker(10 * time.Millisecond)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-t.C:
						hb()
						beats.Add(1)
					}
				}
			},
		}
	}

	cfg := daemon.Config{
		HeartbeatTimeout:   100 * time.Millisecond,
		SubsystemFactories: []daemon.SubsystemFactory{factory},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Wait for a few beats.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && beats.Load() < 5 {
		time.Sleep(5 * time.Millisecond)
	}
	if beats.Load() < 5 {
		t.Errorf("expected >=5 beats from custom subsystem; got %d", beats.Load())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon.Run did not exit within 2s of ctx cancel")
	}
}

// TestRun_RealComposition_SocketAcceptsAttach exercises the real
// daemon + client subsystems end-to-end:
//   1. daemon.Run() boots and binds an attach Unix socket
//   2. test connects as a client + sends an attach header
//   3. daemon writes back the ack frame
// This is a precursor to the full integration test (see
// envelope_emitter_integration_test.go) that also writes JSONL
// records and round-trips them as wire envelopes.
func TestRun_RealComposition_SocketAcceptsAttach(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg := daemon.Config{
		Verbose:          false, // quiet — assertion is on socket behavior
		ProjectsDir:      projectsDir,
		AttachSocketPath: attachSocket,
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	// Wait for socket to appear (max 1s).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(attachSocket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := net.Dial("unix", attachSocket)
	if err != nil {
		t.Fatalf("dial attach socket: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{
		SessionID: "test-sid-123",
		Mode:      string(agent.AttachModeReadOnly),
	}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "dev-test"
	body, _ := json.Marshal(hdr)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Read the ack frame.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := agent.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read ack frame: %v", err)
	}

	var ack agent.AckFrame
	if err := json.Unmarshal(frame, &ack); err != nil {
		t.Fatalf("decode ack: %v (raw=%q)", err, frame)
	}
	if ack.Type != "attach.ack" {
		t.Errorf("ack.Type = %q want attach.ack", ack.Type)
	}
	if ack.SessionID != "test-sid-123" {
		t.Errorf("ack.SessionID = %q want test-sid-123", ack.SessionID)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v; want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon.Run did not exit within 2s of ctx cancel")
	}
}
