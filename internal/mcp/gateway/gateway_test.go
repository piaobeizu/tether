// internal/mcp/gateway/gateway_test.go
package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/permission"
)

// Compile-time interface checks.
var (
	_ gateway.PermissionChecker = allowAll{}
	_ gateway.PermissionChecker = denyAll{}
	_ gateway.PermissionChecker = (*captureChecker)(nil)
)

// --- stubs ---

type allowAll struct{}

func (allowAll) Check(_ context.Context, _ *permission.Request) (*permission.Decision, error) {
	return &permission.Decision{Allow: true}, nil
}

type denyAll struct{}

func (denyAll) Check(_ context.Context, _ *permission.Request) (*permission.Decision, error) {
	return &permission.Decision{Allow: false, Reason: "test deny"}, nil
}

type fakeConn struct{}

func (f *fakeConn) ListTools(_ context.Context) ([]mcp.Tool, error) { return nil, nil }
func (f *fakeConn) CallTool(_ context.Context, _ string, _ json.RawMessage) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "result"}},
	}, nil
}
func (f *fakeConn) Wait() error  { return nil }
func (f *fakeConn) Close() error { return nil }

type fakeManager struct{ conn *fakeConn }

func (m *fakeManager) GetConn(name string) (host.ServerConn, bool) {
	if m.conn != nil {
		return m.conn, true
	}
	return nil, false
}

// --- tests ---

func TestGatewayListTools(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "svc"}, []mcp.Tool{{Name: "echo"}})

	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, allowAll{}, host.NoopLogger())

	tools, err := gw.ListTools(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "svc_echo" {
		t.Fatalf("expected svc_echo, got %+v", tools)
	}
}

func TestGatewayCallToolPermissionDenied(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "svc"}, []mcp.Tool{{Name: "echo"}})

	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, denyAll{}, host.NoopLogger())

	result, err := gw.CallTool(context.Background(), gateway.CallRequest{
		SessionID: "sess-1",
		ToolName:  "svc_echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for permission denied")
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok || tc.Text == "" {
		t.Fatalf("expected non-empty TextContent, got %+v", result.Content[0])
	}
}

func TestGatewayCallToolSuccess(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "svc"}, []mcp.Tool{{Name: "echo"}})

	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, allowAll{}, host.NoopLogger())

	result, err := gw.CallTool(context.Background(), gateway.CallRequest{
		SessionID: "sess-1",
		ToolName:  "svc_echo",
		Arguments: json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatal("expected success result")
	}
}

func TestGatewayCallToolNotFound(t *testing.T) {
	reg := registry.New()
	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, allowAll{}, host.NoopLogger())

	result, err := gw.CallTool(context.Background(), gateway.CallRequest{
		ToolName: "nonexistent_tool",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for unknown tool")
	}
}

func TestGatewayPermissionCheckReceivesCorrectSource(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "polyforge-coding", Prefix: "pf2_coding"},
		[]mcp.Tool{{Name: "commit"}})

	var gotSource string
	checker := &captureChecker{allow: true, captureSource: &gotSource}

	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, checker, host.NoopLogger())

	gw.CallTool(context.Background(), gateway.CallRequest{
		SessionID: "s", ToolName: "pf2_coding_commit",
	})
	if gotSource != "mcp:polyforge-coding" {
		t.Fatalf("expected source=mcp:polyforge-coding, got %q", gotSource)
	}
}

func TestGatewayCallToolServerUnavailable(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "svc"}, []mcp.Tool{{Name: "echo"}})

	// Manager with no conn (simulates server down)
	gw := gateway.New(&fakeManager{conn: nil}, reg, allowAll{}, host.NoopLogger())

	result, err := gw.CallTool(context.Background(), gateway.CallRequest{
		ToolName: "svc_echo",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError=true for unavailable server")
	}
}

type captureChecker struct {
	allow         bool
	captureSource *string
}

func (c *captureChecker) Check(_ context.Context, req *permission.Request) (*permission.Decision, error) {
	if c.captureSource != nil {
		*c.captureSource = req.Source
	}
	return &permission.Decision{Allow: c.allow}, nil
}

type errorChecker struct{}

func (errorChecker) Check(_ context.Context, _ *permission.Request) (*permission.Decision, error) {
	return nil, fmt.Errorf("permission service unavailable")
}

func TestGatewayCallToolPermCheckError(t *testing.T) {
	reg := registry.New()
	reg.Register(host.ServerConfig{Name: "svc"}, []mcp.Tool{{Name: "echo"}})

	checker := &errorChecker{}
	gw := gateway.New(&fakeManager{conn: &fakeConn{}}, reg, checker, host.NoopLogger())

	result, err := gw.CallTool(context.Background(), gateway.CallRequest{
		ToolName: "svc_echo",
	})
	if err == nil {
		t.Fatal("expected non-nil error from permission check failure")
	}
	if result != nil {
		t.Fatal("expected nil result when permission check errors")
	}
}
