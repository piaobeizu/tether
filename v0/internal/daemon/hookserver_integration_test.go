package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/daemon"
)

// TestIntegration_HookServerWireup_PreToolUse_AllowOnce drives the
// full chain end-to-end:
//
//   1. boot daemon with EnableAuthBroker=true (which now also stands
//      up the hookserver + writes settings.json).
//   2. read the settings.json path via OnHookSettingsReady; verify it
//      exists, parses, and routes all 5 hook events at the same
//      loopback baseURL.
//   3. attach as an rw client to the unix socket.
//   4. POST a synthetic PreToolUse payload to the hookserver (i.e.
//      simulate cc).
//   5. assert: (a) the rw client sees an auth.tool-request envelope,
//      (b) the rw client sends back an auth.tool-decision allow-once,
//      (c) the POST returns success with hookSpecificOutput =
//      {permissionDecision: "allow", ...}.
//
// This is the smoke test the P0 follow-up specified — it proves the
// broker actually fires when cc would have hit the loopback URL.
func TestIntegration_HookServerWireup_PreToolUse_AllowOnce(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	hookSettingsDir := filepath.Join(tmp, "cc-settings")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Capture the hook settings path the daemon publishes.
	settingsCh := make(chan string, 1)

	var stderr bytes.Buffer
	cfg := daemon.Config{
		Verbose:           true,
		Stderr:            &stderr,
		ProjectsDir:       projectsDir,
		AttachSocketPath:  attachSocket,
		LockAuditLogPath:  filepath.Join(tmp, "lock.log"),
		EnableAuthBroker:  true,
		HookSettingsDir:   hookSettingsDir,
		// InputSink is required for rw-mode to be accepted (otherwise
		// the daemon auto-downgrades rw → ro and the auth-decision
		// frame interception path doesn't run). The PTY isn't real
		// here — a no-op sink is enough; we never send a non-decision
		// input frame.
		InputSink: func(_ string, _ []byte) error { return nil },
		OnHookSettingsReady: func(path string) {
			settingsCh <- path
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()

	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon.Run = %v want nil", err)
			}
		case <-time.After(3 * time.Second):
			t.Errorf("daemon did not shut down within 3s of ctx cancel; logs:\n%s", stderr.String())
		}
	}()

	// Wait for OnHookSettingsReady (also implies the listener is up
	// and settings.json exists).
	var settingsPath string
	select {
	case settingsPath = <-settingsCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("OnHookSettingsReady never fired; logs:\n%s", stderr.String())
	}

	// (Bonus) verify settings.json shape: all 5 hook events present,
	// every command targets the same loopback baseURL.
	baseURL := assertSettingsFile(t, settingsPath)

	// Wait for unix socket to bind.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(attachSocket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Connect as rw client. We need rw so we can post auth-decision
	// frames back; the broker SubmitDecision routes by requestId, so
	// the rw client doesn't need to hold the lock for decisions
	// (decision frames are intercepted before the lock check — see
	// attach_socket.go::readInputs).
	const sid = "hook-integ-sid"
	conn, err := net.Dial("unix", attachSocket)
	if err != nil {
		t.Fatalf("dial attach: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{
		SessionID: sid,
		Mode:      string(agent.AttachModeReadWrite),
	}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "rw-test-client"
	body, _ := json.Marshal(hdr)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write attach header: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	ackBuf, err := agent.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack agent.AckFrame
	if err := json.Unmarshal(ackBuf, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack.Type != "attach.ack" {
		t.Fatalf("ack.Type=%q want attach.ack (raw=%q)", ack.Type, ackBuf)
	}
	if ack.Mode != "rw" {
		t.Fatalf("ack.Mode=%q want rw (raw=%q) — InputSink wiring may be broken", ack.Mode, ackBuf)
	}

	// Trigger the broker by POSTing a synthetic PreToolUse to the
	// hookserver. We use the URL inside settings.json so this
	// doubles as a sanity check that the URL is reachable.
	preURL := baseURL + "/hooks/pre-tool-use"
	preBody := fmt.Sprintf(
		`{"session_id":%q,"tool_name":"Bash","tool_input":{"command":"echo hi"}}`,
		sid,
	)

	// Run the POST in a goroutine — the broker.Ask blocks until the
	// rw client sends back a decision, so the POST won't return
	// until we read the request envelope + send the decision.
	type postResult struct {
		status int
		body   []byte
		err    error
	}
	resCh := make(chan postResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, preURL, strings.NewReader(preBody))
		if err != nil {
			resCh <- postResult{err: fmt.Errorf("build req: %w", err)}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			resCh <- postResult{err: fmt.Errorf("do: %w", err)}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		resCh <- postResult{status: resp.StatusCode, body: body}
	}()

	// Read the auth.tool-request envelope from the rw client side.
	// The envelope emitter side-channels broker injects through to
	// every subscribed conn; our rw client should observe it.
	var reqID string
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		buf, err := agent.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read auth.tool-request: %v", err)
		}
		var env struct {
			Kind              string         `json:"kind"`
			SessionID         string         `json:"sessionId"`
			PlaintextMetadata map[string]any `json:"plaintextMetadata"`
		}
		if err := json.Unmarshal(buf, &env); err != nil {
			// Skip non-JSON / unrelated frames (shouldn't happen, but
			// defensive — the rw client also gets envelope traffic).
			continue
		}
		if env.Kind != agent.KindAuthToolRequest {
			continue
		}
		if env.SessionID != sid {
			t.Fatalf("envelope sid=%q want %q", env.SessionID, sid)
		}
		ridRaw, ok := env.PlaintextMetadata["requestId"]
		if !ok {
			t.Fatalf("auth.tool-request missing requestId: %s", buf)
		}
		reqID, _ = ridRaw.(string)
		if reqID == "" {
			t.Fatalf("auth.tool-request requestId not a string: %v", ridRaw)
		}
		break
	}

	// Send back allow-once over the rw input channel. The attach
	// server intercepts auth.tool-decision frames before the lock
	// check, so this works without holding the writer lock.
	decision := agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: reqID,
		Decision:  agent.AuthDecisionAllowOnce,
	}
	decBytes, _ := json.Marshal(decision)
	if err := agent.WriteInputFrame(conn, decBytes); err != nil {
		t.Fatalf("write decision frame: %v", err)
	}

	// The POST should now complete with allow.
	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("POST failed: %v", r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("POST status=%d body=%s want 200", r.status, r.body)
		}
		var doc struct {
			HookSpecificOutput struct {
				HookEventName            string `json:"hookEventName"`
				PermissionDecision       string `json:"permissionDecision"`
				PermissionDecisionReason string `json:"permissionDecisionReason"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatalf("decode response %s: %v", r.body, err)
		}
		if doc.HookSpecificOutput.HookEventName != "PreToolUse" {
			t.Errorf("hookEventName=%q want PreToolUse", doc.HookSpecificOutput.HookEventName)
		}
		if doc.HookSpecificOutput.PermissionDecision != "allow" {
			t.Errorf("permissionDecision=%q want allow (reason=%q)",
				doc.HookSpecificOutput.PermissionDecision,
				doc.HookSpecificOutput.PermissionDecisionReason,
			)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("POST did not return within 5s of decision frame; logs:\n%s", stderr.String())
	}
}

// TestIntegration_HookServerWireup_NoInputSink_AuthDecisionFlows is
// the regression test for the dogfood-smoke bug: `tether daemon
// --auth-broker` runs with InputSink=nil (the daemon doesn't spawn
// cc itself), which auto-downgrades rw → ro on attach. Before the
// fix, that downgrade caused readInputs to never start, so the UI's
// auth.tool-decision frame was never read and the broker timed out
// at 60s with fail-closed deny — making the entire auth flow inert
// in production.
//
// Fix (attach_socket.go::serveConn): when AuthBroker is wired,
// readInputs spawns even on ro mode; non-decision frames just drop
// silently inside readInputs.
//
// This test mirrors the AllowOnce test above but explicitly leaves
// InputSink nil to lock in the production scenario.
func TestIntegration_HookServerWireup_NoInputSink_AuthDecisionFlows(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	projectsDir := filepath.Join(tmp, "projects")
	attachSocket := filepath.Join(tmp, "attach.sock")
	hookSettingsDir := filepath.Join(tmp, "cc-settings")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatalf("setup: %v", err)
	}

	settingsCh := make(chan string, 1)
	var stderr bytes.Buffer
	cfg := daemon.Config{
		Verbose:          true,
		Stderr:           &stderr,
		ProjectsDir:      projectsDir,
		AttachSocketPath: attachSocket,
		LockAuditLogPath: filepath.Join(tmp, "lock.log"),
		EnableAuthBroker: true,
		HookSettingsDir:  hookSettingsDir,
		// IMPORTANT: NO InputSink. Mirrors `tether daemon --auth-broker`
		// in production — daemon doesn't spawn cc, so there's no PTY
		// to feed bytes into.
		OnHookSettingsReady: func(p string) { settingsCh <- p },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Errorf("daemon shutdown stalled; logs:\n%s", stderr.String())
		}
	}()

	var settingsPath string
	select {
	case settingsPath = <-settingsCh:
	case <-time.After(3 * time.Second):
		t.Fatalf("OnHookSettingsReady never fired; logs:\n%s", stderr.String())
	}
	baseURL := assertSettingsFile(t, settingsPath)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(attachSocket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	const sid = "no-sink-sid"
	conn, err := net.Dial("unix", attachSocket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	hdr := agent.AttachHeader{SessionID: sid, Mode: string(agent.AttachModeReadWrite)}
	hdr.Client.Kind = "terminal"
	hdr.Client.DeviceID = "no-sink-client"
	body, _ := json.Marshal(hdr)
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write header: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	ackBuf, err := agent.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	var ack agent.AckFrame
	if err := json.Unmarshal(ackBuf, &ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	// Daemon downgrades rw → ro because InputSink is nil — that's
	// expected. The point of this test is that auth-decision frames
	// STILL flow despite the downgrade.
	if ack.Mode != "ro" {
		t.Fatalf("ack.Mode=%q; expected ro (rw should downgrade with no InputSink)", ack.Mode)
	}

	// POST PreToolUse to hookserver.
	preBody := fmt.Sprintf(`{"session_id":%q,"tool_name":"Bash","tool_input":{"command":"echo hi"}}`, sid)
	type postResult struct {
		status int
		body   []byte
		err    error
	}
	resCh := make(chan postResult, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			baseURL+"/hooks/pre-tool-use", strings.NewReader(preBody))
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			resCh <- postResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		resCh <- postResult{status: resp.StatusCode, body: b}
	}()

	// Read the auth.tool-request envelope.
	var reqID string
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		buf, err := agent.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read auth.tool-request: %v\nlogs:\n%s", err, stderr.String())
		}
		var env struct {
			Kind              string         `json:"kind"`
			SessionID         string         `json:"sessionId"`
			PlaintextMetadata map[string]any `json:"plaintextMetadata"`
		}
		if err := json.Unmarshal(buf, &env); err != nil {
			continue
		}
		if env.Kind != agent.KindAuthToolRequest {
			continue
		}
		reqID, _ = env.PlaintextMetadata["requestId"].(string)
		if reqID == "" {
			t.Fatalf("missing requestId: %s", buf)
		}
		break
	}

	// THIS is what the original bug broke: send back a decision frame
	// despite being downgraded to ro. The fix means readInputs is
	// running anyway (because AuthBroker is wired) and intercepts.
	dec := agent.AuthDecisionFrame{
		Type:      agent.KindAuthToolDecision,
		RequestID: reqID,
		Decision:  agent.AuthDecisionAllowOnce,
	}
	decBytes, _ := json.Marshal(dec)
	if err := agent.WriteInputFrame(conn, decBytes); err != nil {
		t.Fatalf("write decision: %v", err)
	}

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("POST failed: %v\nlogs:\n%s", r.err, stderr.String())
		}
		if r.status != http.StatusOK {
			t.Fatalf("POST status=%d body=%s want 200", r.status, r.body)
		}
		var doc struct {
			HookSpecificOutput struct {
				PermissionDecision string `json:"permissionDecision"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if doc.HookSpecificOutput.PermissionDecision != "allow" {
			t.Fatalf("permissionDecision=%q want allow — broker did not receive the decision frame; the ro-mode readInputs spawn fix is broken",
				doc.HookSpecificOutput.PermissionDecision)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("POST timed out — broker likely never received the decision; logs:\n%s", stderr.String())
	}
}

// assertSettingsFile verifies <HookSettingsDir>/settings.json exists,
// parses, and has all 5 hook events pointing at the same
// http://127.0.0.1:N baseURL. Returns that baseURL for the caller.
func assertSettingsFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	stat, _ := os.Stat(path)
	if stat.Mode().Perm()&0o077 != 0 {
		t.Errorf("settings.json too permissive: %v", stat.Mode())
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("settings.json malformed: %v\n%s", err, body)
	}
	wantEvents := []string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "Stop"}
	var baseURL string
	for _, evName := range wantEvents {
		entries, ok := doc.Hooks[evName]
		if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Fatalf("%s not wired in settings.json: %s", evName, body)
		}
		cmd := entries[0].Hooks[0].Command
		if !strings.Contains(cmd, "http://127.0.0.1:") {
			t.Errorf("%s command does not target loopback: %q", evName, cmd)
		}
		// Extract baseURL from the first event and verify the rest
		// match (all 5 events should point at the same loopback URL).
		host := extractBaseURL(cmd)
		if host == "" {
			t.Fatalf("could not extract baseURL from %q", cmd)
		}
		if baseURL == "" {
			baseURL = host
		} else if host != baseURL {
			t.Errorf("event %s baseURL=%q does not match earlier %q", evName, host, baseURL)
		}
	}
	if baseURL == "" {
		t.Fatal("no baseURL extracted from any event")
	}
	return baseURL
}

// extractBaseURL pulls the http://127.0.0.1:N substring out of a
// curl command line. The cc.curlCommand template is:
//
//	curl -sf --max-time 30 -X POST -H 'content-type: application/json' --data-binary @- 'http://127.0.0.1:NNNN/hooks/...'
//
// We grab everything between the last single-quote pair and trim
// the path component.
func extractBaseURL(cmd string) string {
	// Find the URL — last single-quoted segment.
	last := strings.LastIndex(cmd, "'")
	if last <= 0 {
		return ""
	}
	prev := strings.LastIndex(cmd[:last], "'")
	if prev < 0 {
		return ""
	}
	url := cmd[prev+1 : last]
	// Strip "/hooks/<event>" suffix.
	if i := strings.Index(url, "/hooks/"); i > 0 {
		return url[:i]
	}
	return ""
}
