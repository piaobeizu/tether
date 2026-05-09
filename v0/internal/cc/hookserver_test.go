package cc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- handler stub for tests ----------------------------------------

type stubHandlers struct {
	mu               sync.Mutex
	preToolDecision  HookResponse
	preToolErr       error
	preToolPayloads  []json.RawMessage
	postToolPayloads []json.RawMessage
	sessionStartHits int
	userPromptDecision HookResponse
	userPromptErr    error
	stopHits         int
}

func (h *stubHandlers) PreToolUse(_ context.Context, payload json.RawMessage) (HookResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.preToolPayloads = append(h.preToolPayloads, payload)
	return h.preToolDecision, h.preToolErr
}
func (h *stubHandlers) PostToolUse(_ context.Context, payload json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.postToolPayloads = append(h.postToolPayloads, payload)
	return nil
}
func (h *stubHandlers) SessionStart(_ context.Context, _ json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionStartHits++
	return nil
}
func (h *stubHandlers) UserPromptSubmit(_ context.Context, _ json.RawMessage) (HookResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.userPromptDecision, h.userPromptErr
}
func (h *stubHandlers) Stop(_ context.Context, _ json.RawMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopHits++
	return nil
}

// startTestServer boots a hookserver on 127.0.0.1:0 and returns its base URL.
// Caller defers stop.
func startTestServer(t *testing.T, h *stubHandlers) (*HookServer, string) {
	t.Helper()
	srv := NewHookServer(h)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	return srv, "http://" + srv.Addr()
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	buf := &bytes.Buffer{}
	if body != nil {
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	resp, err := http.Post(url, "application/json", buf)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// --- listener / addr ----------------------------------------------

func TestHookServer_StartAssignsLoopbackPort(t *testing.T) {
	srv, _ := startTestServer(t, &stubHandlers{})
	addr := srv.Addr()
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Errorf("Addr should be on loopback only, got %q", addr)
	}
	port := strings.TrimPrefix(addr, "127.0.0.1:")
	if port == "0" || port == "" {
		t.Errorf("Addr port not assigned: %q", addr)
	}
}

// --- /hooks/* paths ------------------------------------------------

func TestHookServer_PreToolUse_Allow(t *testing.T) {
	h := &stubHandlers{
		preToolDecision: HookResponse{Decision: "allow", Reason: "test ok"},
	}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/pre-tool-use", map[string]any{
		"tool_name": "Bash",
		"input":     map[string]string{"command": "echo hi"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	hso, ok := body["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("response missing hookSpecificOutput: %+v", body)
	}
	if hso["hookEventName"] != "PreToolUse" {
		t.Errorf("hookEventName: got %v, want PreToolUse", hso["hookEventName"])
	}
	if hso["permissionDecision"] != "allow" {
		t.Errorf("permissionDecision: got %v, want allow", hso["permissionDecision"])
	}
	if hso["permissionDecisionReason"] != "test ok" {
		t.Errorf("permissionDecisionReason: got %v", hso["permissionDecisionReason"])
	}
	if len(h.preToolPayloads) != 1 {
		t.Errorf("handler invocation count: got %d, want 1", len(h.preToolPayloads))
	}
}

func TestHookServer_PreToolUse_Deny(t *testing.T) {
	h := &stubHandlers{preToolDecision: HookResponse{Decision: "deny", Reason: "danger"}}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/pre-tool-use", map[string]any{"tool_name": "Bash"})
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	hso := body["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" {
		t.Errorf("decision: %v", hso["permissionDecision"])
	}
}

func TestHookServer_PreToolUse_HandlerError(t *testing.T) {
	h := &stubHandlers{preToolErr: errors.New("upstream gone")}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/pre-tool-use", map[string]any{"tool_name": "X"})
	defer resp.Body.Close()
	if resp.StatusCode != 500 {
		t.Errorf("handler error → status: got %d, want 500", resp.StatusCode)
	}
}

func TestHookServer_PostToolUse_ObserverNoBody(t *testing.T) {
	h := &stubHandlers{}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/post-tool-use", map[string]any{"tool_name": "Bash", "duration_ms": 42})
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("PostToolUse: got %d, want 204 (observer hook, no decision body)", resp.StatusCode)
	}
	if len(h.postToolPayloads) != 1 {
		t.Errorf("PostToolUse handler invocation count: got %d", len(h.postToolPayloads))
	}
}

func TestHookServer_SessionStart(t *testing.T) {
	h := &stubHandlers{}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/session-start", map[string]any{"session_id": "x"})
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("SessionStart: got %d, want 204", resp.StatusCode)
	}
	if h.sessionStartHits != 1 {
		t.Errorf("hits: got %d", h.sessionStartHits)
	}
}

func TestHookServer_UserPromptSubmit_Allow(t *testing.T) {
	h := &stubHandlers{userPromptDecision: HookResponse{Decision: "allow"}}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/user-prompt-submit", map[string]any{"prompt": "hi"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status: %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	hso := body["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "UserPromptSubmit" {
		t.Errorf("hookEventName: %v", hso["hookEventName"])
	}
}

func TestHookServer_Stop(t *testing.T) {
	h := &stubHandlers{}
	_, base := startTestServer(t, h)
	resp := postJSON(t, base+"/hooks/stop", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("Stop: got %d, want 204", resp.StatusCode)
	}
	if h.stopHits != 1 {
		t.Errorf("hits: got %d", h.stopHits)
	}
}

// --- /blob/* — 501 not implemented in v0.1 -------------------------

func TestHookServer_BlobReturns501(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{})
	resp := postJSON(t, base+"/blob/some-sha256", map[string]any{"file": "x"})
	defer resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Errorf("/blob/* should be 501 (D-18 reserved namespace, v0.2 Phase 1): got %d", resp.StatusCode)
	}
}

// --- prefix invariant + 404 ---------------------------------------

func TestHookServer_HookPathRequiresPrefix(t *testing.T) {
	// /pre-tool-use without /hooks/ prefix → 404 (D-18: hooks live under /hooks/).
	_, base := startTestServer(t, &stubHandlers{})
	resp := postJSON(t, base+"/pre-tool-use", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("/pre-tool-use without /hooks/ prefix: got %d, want 404", resp.StatusCode)
	}
}

func TestHookServer_UnknownHookSubpath(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{})
	resp := postJSON(t, base+"/hooks/unknown-event", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("/hooks/unknown-event: got %d, want 404", resp.StatusCode)
	}
}

func TestHookServer_RootPath(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{})
	resp := postJSON(t, base+"/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("/ (root): got %d, want 404", resp.StatusCode)
	}
}

// --- payload preserved verbatim ------------------------------------

func TestHookServer_PayloadPassthrough(t *testing.T) {
	h := &stubHandlers{preToolDecision: HookResponse{Decision: "allow"}}
	_, base := startTestServer(t, h)
	originalBody := `{"tool_name":"Read","input":{"file_path":"/etc/passwd"},"extra_unknown":42}`
	resp, err := http.Post(base+"/hooks/pre-tool-use", "application/json", strings.NewReader(originalBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if len(h.preToolPayloads) != 1 {
		t.Fatalf("payload count: %d", len(h.preToolPayloads))
	}
	got := string(h.preToolPayloads[0])
	// Whitespace tolerated by json (we receive verbatim bytes), but content
	// must be intact — including unknown field.
	if !strings.Contains(got, `"extra_unknown":42`) {
		t.Errorf("payload lost extra_unknown: %q", got)
	}
}

// --- Stop / lifecycle ---------------------------------------------

func TestHookServer_StopBlocksUntilDone(t *testing.T) {
	srv := NewHookServer(&stubHandlers{})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("Stop: %v", err)
	}
	// Second Stop should be idempotent.
	if err := srv.Stop(stopCtx); err != nil {
		t.Errorf("double-Stop: %v", err)
	}
}

func TestHookServer_PostStartAddrStable(t *testing.T) {
	srv, _ := startTestServer(t, &stubHandlers{})
	a1 := srv.Addr()
	a2 := srv.Addr()
	if a1 != a2 {
		t.Errorf("Addr changed across calls: %q vs %q", a1, a2)
	}
}

// --- method gating: cc only POSTs to hooks ------------------------

func TestHookServer_GETOnHookReturns405(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{})
	resp, err := http.Get(base + "/hooks/pre-tool-use")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /hooks/pre-tool-use: got %d, want 405", resp.StatusCode)
	}
}

// --- review fix: oversize body returns 413 -------------------------

func TestHookServer_OversizeBodyReturns413(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{preToolDecision: HookResponse{Decision: "allow"}})
	// 9 MiB > MaxRequestBodyBytes (8 MiB).
	body := bytes.Repeat([]byte("x"), 9<<20)
	resp, err := http.Post(base+"/hooks/pre-tool-use", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize body: got %d, want 413", resp.StatusCode)
	}
}

// --- review fix: noopHandlers fail-closed --------------------------

func TestNoopHandlers_PreToolUse_FailClosed(t *testing.T) {
	srv := NewHookServer(nil) // explicit nil → noopHandlers default
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	resp := postJSON(t, "http://"+srv.Addr()+"/hooks/pre-tool-use", map[string]any{"tool_name": "Bash"})
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	hso, _ := body["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" {
		t.Errorf("noopHandlers PreToolUse: got %v, want deny (fail-closed default)", hso["permissionDecision"])
	}
	if !strings.Contains(hso["permissionDecisionReason"].(string), "not configured") {
		t.Errorf("noop deny reason should explain misconfig: %v", hso["permissionDecisionReason"])
	}
}

func TestNoopHandlers_UserPromptSubmit_FailClosed(t *testing.T) {
	srv := NewHookServer(nil)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})
	resp := postJSON(t, "http://"+srv.Addr()+"/hooks/user-prompt-submit", map[string]any{"prompt": "hi"})
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	hso, _ := body["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" {
		t.Errorf("noopHandlers UserPromptSubmit: got %v, want deny", hso["permissionDecision"])
	}
}

// --- review fix: Stop awaits Serve goroutine -----------------------

func TestHookServer_StopAwaitsServeGoroutineExit(t *testing.T) {
	srv := NewHookServer(&stubHandlers{})
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("stop: %v", err)
	}
	// After Stop returns, the listener address must be unreachable —
	// confirms the Serve goroutine has fully exited (closed the listener).
	addr := srv.Addr()
	conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Errorf("listener still accepting connections after Stop: addr=%s", addr)
	}
}

// --- review fix: Stop-before-Start poisons subsequent Start --------

func TestHookServer_StopBeforeStart_PoisonsStart(t *testing.T) {
	srv := NewHookServer(&stubHandlers{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Stop on a never-started server is a no-op (returns nil) but
	// poisons the server.
	if err := srv.Stop(ctx); err != nil {
		t.Errorf("stop before start: got %v want nil", err)
	}
	if err := srv.Start(ctx); !errors.Is(err, ErrServerStopped) {
		t.Errorf("Start after Stop: got %v, want ErrServerStopped", err)
	}
}

// --- review fix: ServeErr() reports nil for clean shutdown --------

func TestHookServer_ServeErr_NilForCleanShutdown(t *testing.T) {
	srv, _ := startTestServer(t, &stubHandlers{})
	// Wait briefly so server is fully up before stop.
	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = srv.Stop(ctx)
	if err := srv.ServeErr(); err != nil {
		t.Errorf("ServeErr after clean shutdown: got %v want nil", err)
	}
}

// --- review fix: concurrent POST safety ---------------------------

func TestHookServer_ConcurrentPOSTs(t *testing.T) {
	h := &stubHandlers{preToolDecision: HookResponse{Decision: "allow"}}
	_, base := startTestServer(t, h)
	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			resp := postJSON(t, base+"/hooks/pre-tool-use", map[string]any{"tool_name": "Bash"})
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.preToolPayloads) != N {
		t.Errorf("concurrent POSTs: got %d invocations, want %d", len(h.preToolPayloads), N)
	}
}

// --- review fix: /blob and /blob/ edge cases ----------------------

func TestHookServer_BlobNoTrailingSlash_Redirected(t *testing.T) {
	// http.ServeMux registered "/blob/" — a request to "/blob" gets a
	// 301 redirect to "/blob/". Verify the redirect target hits 501.
	_, base := startTestServer(t, &stubHandlers{})
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Post(base+"/blob", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMovedPermanently && resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusPermanentRedirect && resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("/blob (no slash): got %d, want 301/307/308 redirect or direct 501", resp.StatusCode)
	}
}

func TestHookServer_BlobBareSlash_Returns501(t *testing.T) {
	_, base := startTestServer(t, &stubHandlers{})
	resp := postJSON(t, base+"/blob/", nil)
	defer resp.Body.Close()
	if resp.StatusCode != 501 {
		t.Errorf("/blob/ (bare slash): got %d, want 501", resp.StatusCode)
	}
}
