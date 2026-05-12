package server_test

import (
	"bytes"
	"context"
	"encoding/json" // for startInstanceReq body marshaling
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/permission"
	"github.com/piaobeizu/tether/internal/server"
)

// setupTaskMCPServer returns an httptest.Server with the task MCP REST API
// mounted, plus the LifecycleManager so callers can inspect running instances.
func setupTaskMCPServer(t *testing.T) (*httptest.Server, *mcplifecycle.LifecycleManager) {
	t.Helper()
	lm := mcplifecycle.New()
	pm := permission.New()
	mux := http.NewServeMux()
	server.RegisterTaskMCPAPI(mux, lm, pm)
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		lm.StopAll(ctx)
	})
	return ts, lm
}

// startInstanceReq calls POST /api/v1/tasks/{id}/mcp with skip_inject=true.
func startInstanceReq(t *testing.T, ts *httptest.Server, taskID, slug, wsRoot string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"slug":         slug,
		"ws_root":      wsRoot,
		"skip_inject":  true,
	})
	resp, err := http.Post(
		fmt.Sprintf("%s/api/v1/tasks/%s/mcp", ts.URL, taskID),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST task mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST task mcp: expected 200, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func TestTaskMCPLifecycle_StartAndStop(t *testing.T) {
	ts, lm := setupTaskMCPServer(t)
	wsRoot := t.TempDir()
	taskID := "t-smoke-001"

	// Start an MCPInstance via REST.
	out := startInstanceReq(t, ts, taskID, "smoke", wsRoot)
	port := int(out["port"].(float64))
	token := out["token"].(string)
	if port == 0 || token == "" {
		t.Fatalf("expected non-zero port and token, got port=%d token=%q", port, token)
	}

	// Verify LifecycleManager registered it.
	if inst, ok := lm.Get(taskID); !ok || inst.Port != port {
		t.Fatalf("instance not found in manager after start")
	}

	// GET inspect endpoint.
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/tasks/%s/mcp", ts.URL, taskID))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d", resp.StatusCode)
	}

	// DELETE stops the instance.
	req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/v1/tasks/%s/mcp", ts.URL, taskID), nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE: expected 204, got %d", delResp.StatusCode)
	}

	// Instance no longer in manager.
	if _, ok := lm.Get(taskID); ok {
		t.Fatal("instance still in manager after DELETE")
	}
}

func TestTaskMCPLifecycle_ToolCallEndToEnd(t *testing.T) {
	ts, _ := setupTaskMCPServer(t)
	wsRoot := t.TempDir()
	taskID := "t-e2e-001"

	// Start instance.
	out := startInstanceReq(t, ts, taskID, "e2e", wsRoot)
	port := int(out["port"].(float64))
	token := out["token"].(string)

	// Wait for loopback to be ready via retry-dial instead of a fixed sleep.
	waitForPort(t, port, 200*time.Millisecond)

	// Connect MCP client to the per-task loopback.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "smoke-test"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		HTTPClient: &http.Client{
			Transport: &bearerTransport{token: token, base: http.DefaultTransport},
		},
		DisableStandaloneSSE: true,
	}
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP connect: %v", err)
	}
	defer sess.Close()

	// workspace_run_shell must be listed.
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if !containsTool(tools.Tools, "workspace_run_shell") {
		t.Fatalf("workspace_run_shell not in tools list; got %v", toolNames(tools.Tools))
	}

	// Call workspace_run_shell — echo a known string.
	// Arguments must be map[string]any (not json.RawMessage) — go-sdk marshals
	// the any value to JSON; passing []byte would produce base64, not an object.
	result, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "workspace_run_shell",
		Arguments: map[string]any{"command": "echo e2e-ok"},
	})
	if err != nil {
		t.Fatalf("CallTool workspace_run_shell: %v", err)
	}
	if result.IsError {
		errText := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				errText = tc.Text
			}
		}
		t.Fatalf("tool returned error: %s", errText)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "e2e-ok") {
		t.Fatalf("expected stdout to contain 'e2e-ok', got %q", text)
	}
}

func TestTaskMCPLifecycle_MissingSlugRejected(t *testing.T) {
	ts, _ := setupTaskMCPServer(t)
	body, _ := json.Marshal(map[string]any{
		"ws_root":     t.TempDir(),
		"skip_inject": true,
	})
	resp, err := http.Post(
		fmt.Sprintf("%s/api/v1/tasks/t-bad/mcp", ts.URL),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing slug, got %d", resp.StatusCode)
	}
}

// waitForPort retries a TCP dial until the port accepts connections or deadline.
func waitForPort(t *testing.T, port int, deadline time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 5*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("port %d not ready after %v", port, deadline)
}
