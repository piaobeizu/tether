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
)

// BenchmarkGatewayCallTool measures the permission-allowed tool dispatch path:
// permission check (in-memory allow) → registry lookup → fake conn.CallTool.
// This is the hot path for every MCP tool call tether proxies.
func BenchmarkGatewayCallTool(b *testing.B) {
	reg := registry.New()
	conn := &fakeConn{}
	// prefix = "bench_srv_" → tool name = "bench_srv_echo"
	if err := reg.Register(host.ServerConfig{Name: "bench-srv"}, []mcp.Tool{{Name: "echo"}}); err != nil {
		b.Fatal(err)
	}

	gw := gateway.New(&fakeManager{conn: conn}, reg, allowAll{}, host.NoopLogger())
	args, _ := json.Marshal(map[string]string{"text": "hello"})
	req := gateway.CallRequest{
		SessionID: "bench-session",
		ToolName:  "bench_srv_echo",
		Arguments: args,
	}
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		if _, err := gw.CallTool(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGatewayListTools measures the cost of listing all registered tools.
// Registers 10 tools to simulate a realistic MCP server.
func BenchmarkGatewayListTools(b *testing.B) {
	reg := registry.New()
	conn := &fakeConn{}
	tools := make([]mcp.Tool, 10)
	for i := range tools {
		tools[i] = mcp.Tool{Name: fmt.Sprintf("tool_%d", i)}
	}
	if err := reg.Register(host.ServerConfig{Name: "bench-srv"}, tools); err != nil {
		b.Fatal(err)
	}

	gw := gateway.New(&fakeManager{conn: conn}, reg, allowAll{}, host.NoopLogger())
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		if _, err := gw.ListTools(ctx, "bench-session"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPermissionDenied measures the fast-path when permission is denied
// before any conn.CallTool is reached.
func BenchmarkPermissionDenied(b *testing.B) {
	reg := registry.New()
	conn := &fakeConn{}
	_ = reg.Register(host.ServerConfig{Name: "bench-srv"}, []mcp.Tool{{Name: "echo"}})

	gw := gateway.New(&fakeManager{conn: conn}, reg, denyAll{}, host.NoopLogger())
	args, _ := json.Marshal(map[string]string{"text": "hello"})
	req := gateway.CallRequest{
		SessionID: "bench-session",
		ToolName:  "bench_srv_echo",
		Arguments: args,
	}
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		_, _ = gw.CallTool(ctx, req) // expected to return permission denied
	}
}
