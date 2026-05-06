package agent_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
)

// TestIntegration_DaemonAttachJSONLToEnvelopes exercises the full
// Epic-3 integration slice end-to-end:
//
//   1. spin a daemon rooted at a temp ProjectsDir + attach socket.
//   2. connect to the attach socket as a client (read-only).
//   3. simulate a "fake cc" by writing a deterministic JSONL sequence
//      to <projectsDir>/<sid>.jsonl: 1 system, 1 user, 1 assistant,
//      1 hook attachment, 1 permission-mode state.
//   4. assert the client receives a mix of EVENT / HOOK / STATE
//      envelopes in order.
//
// The test does NOT depend on a real cc binary — the JSONL watcher
// is the source of truth and the rest of the daemon flow (socket
// framing, envelope mapping) doesn't care whose hand wrote the
// JSONL line. Run with `-race` to catch any goroutine fan-out
// concurrency bugs.
func TestIntegration_DaemonAttachJSONLToEnvelopes(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Boot the daemon.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Config{
			ProjectsDir:      projectsDir,
			AttachSocketPath: attachSocket,
			LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		})
	}()

	// Wait for socket to appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(attachSocket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Connect + send header.
	const sid = "01abc-int-test-sid"
	conn, err := net.Dial("unix", attachSocket)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{
		SessionID: sid,
		Mode:      string(agent.AttachModeReadOnly),
	}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "test-dev"
	body, _ := json.Marshal(hdr)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write header: %v", err)
	}

	// Read ack.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := agent.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack agent.AckFrame
	if err := json.Unmarshal(frame, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.Type != "attach.ack" {
		t.Fatalf("ack.Type = %q want attach.ack (raw=%q)", ack.Type, frame)
	}

	// Now write JSONL records to <projectsDir>/<sid>.jsonl.
	// The watcher is rooted at projectsDir; new file should be
	// picked up by the fsnotify Create event.
	jsonlPath := filepath.Join(projectsDir, sid+".jsonl")
	records := []string{
		// EVENT: user message
		`{"type":"user","uuid":"u1","sessionId":"` + sid + `","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		// EVENT: assistant message
		`{"type":"assistant","uuid":"u2","sessionId":"` + sid + `","message":{"role":"assistant","content":[{"type":"text","text":"hi back"}]}}`,
		// HOOK: PreToolUse attachment
		`{"type":"attachment","uuid":"h1","sessionId":"` + sid + `","attachment":{"type":"hook","hookEvent":"PreToolUse","hookName":"approve-shell","toolUseID":"tool-001"}}`,
		// STATE: permission-mode change
		`{"type":"permission-mode","sessionId":"` + sid + `","permissionMode":"plan"}`,
	}

	f, err := os.Create(jsonlPath)
	if err != nil {
		t.Fatalf("create jsonl: %v", err)
	}
	for _, line := range records {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("write jsonl: %v", err)
		}
	}
	_ = f.Sync()
	_ = f.Close()

	// Read envelopes from the attach socket. We expect to receive
	// 4 frames (one per record). Allow up to 3s — fsnotify scheduling
	// is fast but not instant, especially on first-create where we
	// also need the openFile + readFile path to fire.
	type observed struct {
		Kind       string `json:"kind"`
		SessionID  string `json:"sessionId"`
		SourceUUID string `json:"sourceUuid"`
	}
	got := make([]observed, 0, len(records))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for len(got) < len(records) {
		buf, err := agent.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read envelope frame %d: %v (so far: %+v)", len(got), err, got)
		}
		var env observed
		if err := json.Unmarshal(buf, &env); err != nil {
			t.Fatalf("decode envelope %d: %v (raw=%q)", len(got), err, buf)
		}
		got = append(got, env)
	}

	// Verify: every envelope is for our session.
	for i, e := range got {
		if e.SessionID != sid {
			t.Errorf("env[%d] SessionID = %q want %q", i, e.SessionID, sid)
		}
	}

	// Verify the kind sequence: 2x agent-event, 1x hook-event,
	// 1x session.state.
	wantKindCount := map[string]int{
		"output.agent-event": 2,
		"output.hook-event":  1,
		"session.state":      1,
	}
	gotKindCount := map[string]int{}
	for _, e := range got {
		gotKindCount[e.Kind]++
	}
	for k, n := range wantKindCount {
		if gotKindCount[k] != n {
			t.Errorf("kind %q: got %d want %d (full kinds: %+v)", k, gotKindCount[k], n, gotKindCount)
		}
	}

	// Verify uuids landed verbatim for the EVENT/HOOK records.
	wantUUIDs := map[string]bool{"u1": true, "u2": true, "h1": true}
	for _, e := range got {
		if e.SourceUUID != "" {
			if !wantUUIDs[e.SourceUUID] {
				t.Errorf("unexpected sourceUuid %q in envelope (kind=%q)", e.SourceUUID, e.Kind)
			}
			delete(wantUUIDs, e.SourceUUID)
		}
	}
	if len(wantUUIDs) > 0 {
		missing := make([]string, 0, len(wantUUIDs))
		for u := range wantUUIDs {
			missing = append(missing, u)
		}
		t.Errorf("never observed uuids: %v", missing)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("daemon.Run = %v want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not shut down within 2s of ctx cancel")
	}
}

// TestIntegration_AttachLockGate verifies the rw-mode lock gate:
//   1. boot daemon with InputSink that records what arrives.
//   2. attach as rw client A, send 1 input frame — lock acquires + sink fires.
//   3. attach as rw client B, send 1 input frame — first byte fails
//      with attach.lock-denied because A still holds the lock and
//      the auto-release window hasn't elapsed.
func TestIntegration_AttachLockGate(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	type sinkEvent struct {
		sid  string
		data []byte
	}
	sinkCh := make(chan sinkEvent, 16)
	cfg := daemon.Config{
		ProjectsDir:      projectsDir,
		AttachSocketPath: attachSocket,
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		InputSink: func(sid string, data []byte) error {
			sinkCh <- sinkEvent{sid: sid, data: append([]byte(nil), data...)}
			return nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(attachSocket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	const sid = "lock-gate-sid"

	dialAttach := func(t *testing.T, deviceID string, mode agent.AttachMode) net.Conn {
		t.Helper()
		c, err := net.Dial("unix", attachSocket)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		hdr := agent.AttachHeader{SessionID: sid, Mode: string(mode)}
		hdr.Client.Kind = "terminal"
		hdr.Client.DeviceID = deviceID
		body, _ := json.Marshal(hdr)
		_, _ = c.Write(append(body, '\n'))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = agent.ReadFrame(c) // ack
		if err != nil {
			t.Fatalf("ack frame: %v", err)
		}
		return c
	}

	connA := dialAttach(t, "device-A", agent.AttachModeReadWrite)
	defer connA.Close()

	if err := agent.WriteInputFrame(connA, []byte("hello-A")); err != nil {
		t.Fatalf("write input from A: %v", err)
	}

	// Sink should fire for A.
	select {
	case ev := <-sinkCh:
		if string(ev.data) != "hello-A" {
			t.Errorf("sink for A: got %q want %q", string(ev.data), "hello-A")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not receive A's input")
	}

	// B attaches as rw with a different deviceID. Its first input
	// MUST be rejected with attach.lock-denied because A still holds
	// the lock (auto-release window is 60s).
	connB := dialAttach(t, "device-B", agent.AttachModeReadWrite)
	defer connB.Close()

	if err := agent.WriteInputFrame(connB, []byte("hello-B")); err != nil {
		t.Fatalf("write input from B: %v", err)
	}

	_ = connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := agent.ReadFrame(connB)
	if err != nil {
		t.Fatalf("read denial frame for B: %v", err)
	}
	var denied agent.LockDeniedFrame
	if err := json.Unmarshal(frame, &denied); err != nil {
		t.Fatalf("decode denial: %v (raw=%q)", err, frame)
	}
	if denied.Type != "attach.lock-denied" {
		t.Errorf("denied.Type = %q want attach.lock-denied", denied.Type)
	}
	if !strings.Contains(strings.ToLower(denied.Reason), "lock") &&
		!strings.Contains(strings.ToLower(denied.Reason), "use") {
		t.Errorf("denied.Reason should mention lock/use; got %q", denied.Reason)
	}
	if denied.Holder.DeviceID != "device-A" {
		t.Errorf("denied.Holder.DeviceID = %q want device-A", denied.Holder.DeviceID)
	}

	// B's attempt MUST NOT have reached the sink.
	select {
	case ev := <-sinkCh:
		t.Errorf("sink received unexpected event from B: %q", string(ev.data))
	case <-time.After(150 * time.Millisecond):
		// good — gated.
	}

	// Spec §11.D audit log: A's implicit acquire MUST have persisted to
	// the configured lock.log. We don't assert exact line content (the
	// JSONL schema is covered in lock package tests); just that the
	// file exists and is non-empty by the time B is denied.
	auditPath := filepath.Join(tmp, "lock.log")
	if st, statErr := os.Stat(auditPath); statErr != nil {
		t.Errorf("expected lock audit log at %q after acquire: %v", auditPath, statErr)
	} else if st.Size() == 0 {
		t.Errorf("lock audit log %q is empty after acquire", auditPath)
	} else if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("lock audit log mode: got %o want 0600", mode)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}
