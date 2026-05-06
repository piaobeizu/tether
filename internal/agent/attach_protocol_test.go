// Phase 9 protocol lock-in: explicit wire-shape tests for the attach
// socket. These complement integration_test.go (which exercises the
// full daemon → JSONL watcher → envelope path) by isolating the
// AttachServer + a hand-rolled "Rust-shaped" client that just speaks
// the wire bytes — same shape the tether-app Tauri command issues
// from src-tauri/src/attach/mod.rs.
//
// The point: if the wire format ever drifts (header newline contract,
// 4-BE length prefix, ack frame shape), these tests fail BEFORE the
// integration test does, so the breakage is localized.

package agent_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/lock"
)

// TestAttachProtocol_HeaderAckDrop simulates the minimum wire dance the
// tether-app Rust bridge issues:
//
//   1. dial Unix socket
//   2. write JSON header line + '\n'
//   3. read 4-byte BE length-prefixed JSON ack frame
//   4. close — daemon must clean up without error
//
// The test does NOT push any envelopes through; it locks in just the
// connect-time handshake.
func TestAttachProtocol_HeaderAckDrop(t *testing.T) {
	t.Parallel()

	srv, sockPath := newTestAttachServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ctx) }()
	// Cleanup order: cancel ctx FIRST so serveConn's select unblocks,
	// then Close (idempotent) so the listener tears down + waits for
	// per-conn goroutines, then drain srvDone.
	defer func() {
		cancel()
		_ = srv.Close()
		select {
		case <-srvDone:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not shut down within 2s of ctx cancel")
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{SessionID: "sid-rust-123", Mode: string(agent.AttachModeReadOnly)}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "rust-bridge-test"
	body, _ := json.Marshal(hdr)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write header: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := agent.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}

	var ack agent.AckFrame
	if err := json.Unmarshal(frame, &ack); err != nil {
		t.Fatalf("ack JSON parse: %v (raw=%q)", err, frame)
	}
	if ack.Type != "attach.ack" {
		t.Errorf("ack.Type = %q want attach.ack", ack.Type)
	}
	if ack.SessionID != "sid-rust-123" {
		t.Errorf("ack.SessionID = %q want sid-rust-123", ack.SessionID)
	}
	if ack.Mode != string(agent.AttachModeReadOnly) {
		t.Errorf("ack.Mode = %q want ro", ack.Mode)
	}
}

// TestAttachProtocol_FrameSizePrefix sanity-checks the 4-byte BE length
// prefix the Rust bridge depends on. We pull the raw bytes (NOT through
// ReadFrame) so a regression in the helper itself is also caught.
func TestAttachProtocol_FrameSizePrefix(t *testing.T) {
	t.Parallel()

	srv, sockPath := newTestAttachServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ctx) }()
	// Cleanup order: cancel ctx FIRST so serveConn's select unblocks,
	// then Close (idempotent) so the listener tears down + waits for
	// per-conn goroutines, then drain srvDone.
	defer func() {
		cancel()
		_ = srv.Close()
		select {
		case <-srvDone:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not shut down within 2s of ctx cancel")
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{SessionID: "sid-frame-prefix", Mode: "ro"}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "rust-bridge-test"
	body, _ := json.Marshal(hdr)
	_, _ = conn.Write(append(body, '\n'))

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var lenBuf [4]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		t.Fatalf("read length prefix: %v", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 || n > 64*1024 {
		// 64K is a sanity ceiling for the ack frame; the real cap is
		// 1MB but the ack body is well under 200B so anything > 64K
		// here is a wire-shape regression.
		t.Errorf("ack length prefix = %d, want 0 < n <= 65536", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatalf("payload not JSON: %v (raw=%q)", err, payload)
	}
	if v["type"] != "attach.ack" {
		t.Errorf("payload.type = %v want attach.ack", v["type"])
	}
}

// TestAttachProtocol_HeaderTooLargeIsRejected — the daemon caps the
// newline-terminated header read at MaxHeaderBytes (64KB). A buggy
// client that streams without ever sending '\n' must NOT be able to
// OOM the daemon. This test floods the socket with junk under the cap
// + verifies the daemon drops the conn instead of panicking. The Rust
// bridge sends well under 1KB so this is a guardrail, not a behavior
// the bridge itself depends on — but locking it in keeps the daemon
// safe regardless of who connects.
func TestAttachProtocol_HeaderTooLargeIsRejected(t *testing.T) {
	t.Parallel()

	// Capture daemon-side errors so we can assert the daemon rejected
	// for the SPECIFIC reason we expect (ErrHeaderTooLarge), not just
	// "conn dropped for any reason" — a buggy daemon panic would also
	// drop the conn.
	var (
		errMu     sync.Mutex
		caughtErr error
	)
	srv, sockPath := newTestAttachServerWithOpts(t, func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		// First error wins — subsequent close-related errors get
		// folded so we keep the root cause.
		if caughtErr == nil {
			caughtErr = err
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Serve(ctx) }()
	// Cleanup order: cancel ctx FIRST so serveConn's select unblocks,
	// then Close (idempotent) so the listener tears down + waits for
	// per-conn goroutines, then drain srvDone.
	defer func() {
		cancel()
		_ = srv.Close()
		select {
		case <-srvDone:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not shut down within 2s of ctx cancel")
		}
	}()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send 65KB+1024 of garbage with no newline. The daemon should bail
	// at byte MaxHeaderBytes and close the conn.
	junk := make([]byte, agent.MaxHeaderBytes+1024)
	for i := range junk {
		junk[i] = 'x'
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	// Best-effort write; the conn may close mid-write once the daemon
	// hits the cap. Ignore the error.
	_, _ = conn.Write(junk)

	// Reading should now hit EOF / closed-conn since the daemon
	// rejected our oversized header.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
		t.Errorf("daemon kept conn open after oversized header (got %d bytes: %q)", n, buf[:n])
	}

	// Hard assertion: the OnError callback fired with ErrHeaderTooLarge.
	// Conn-level close events arrive before serveConn's reportError on
	// some kernels — wait briefly for the error to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		errMu.Lock()
		got := caughtErr
		errMu.Unlock()
		if got != nil {
			if !errors.Is(got, agent.ErrHeaderTooLarge) {
				t.Errorf("expected ErrHeaderTooLarge, got %v", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("OnError never fired — daemon dropped the conn for some other reason")
}

// --- helpers ---

func newTestAttachServer(t *testing.T) (*agent.AttachServer, string) {
	return newTestAttachServerWithOpts(t, nil)
}

// newTestAttachServerWithOpts is the variant for tests that need to
// observe daemon-side errors (e.g. asserting ErrHeaderTooLarge fires
// for the right reason, not just "conn dropped"). Pass nil for
// onError to get the bare-bones server.
func newTestAttachServerWithOpts(t *testing.T, onError func(error)) (*agent.AttachServer, string) {
	t.Helper()
	tmp := t.TempDir()
	sockPath := filepath.Join(tmp, "attach.sock")
	projDir := filepath.Join(tmp, "projects")
	if err := os.MkdirAll(projDir, 0o700); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}

	em, err := agent.NewEnvelopeEmitter(context.Background(), agent.EmitterConfig{
		ProjectsDir: projDir,
	})
	if err != nil {
		t.Fatalf("NewEnvelopeEmitter: %v", err)
	}
	lk := lock.New()

	srv, err := agent.NewAttachServer(agent.AttachServerConfig{
		SocketPath: sockPath,
		Emitter:    em,
		Lock:       lk,
		SocketPerm: 0o600,
		OnError:    onError,
	})
	if err != nil {
		_ = em.Close()
		t.Fatalf("NewAttachServer: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = em.Close()
		_ = os.Remove(sockPath)
	})
	return srv, sockPath
}
