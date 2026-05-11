// internal/mcp/gateway/integration_test.go
package gateway_test

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
)

type echoInput struct {
	Text string `json:"text"`
}

func TestGatewayIntegration_InProcessServer(t *testing.T) {
	ctx := context.Background()

	// Build an in-process MCP server with an echo tool.
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-srv", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "echo", Description: "Echo text"},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: in.Text}},
			}, struct{}{}, nil
		},
	)

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	go func() { srv.Connect(ctx, serverTransport, nil) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "tether-test", Version: "v0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	// Wire through registry + gateway.
	reg := registry.New()
	cfg := host.ServerConfig{Name: "test-srv", Prefix: "tsrv"}
	conn := host.NewLiveConn(session)
	tools, err := conn.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if err := reg.Register(cfg, tools); err != nil {
		t.Fatalf("Register: %v", err)
	}

	mgr := &staticManager{conn: conn, serverName: "test-srv"}
	gw := gateway.New(mgr, reg, allowAll{}, host.NoopLogger())

	// Verify ListTools returns prefixed name.
	listed, err := gw.ListTools(ctx, "sess")
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "tsrv_echo" {
		t.Fatalf("expected tsrv_echo, got %+v", listed)
	}

	// Verify CallTool round-trips through the real go-sdk session.
	result, err := gw.CallTool(ctx, gateway.CallRequest{
		SessionID: "sess",
		ToolName:  "tsrv_echo",
		Arguments: map[string]any{"text": "hello mcp"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected success, got IsError=true: %+v", result)
	}
	tc, ok := result.Content[0].(*mcp.TextContent)
	if !ok || tc.Text != "hello mcp" {
		t.Fatalf("expected echo 'hello mcp', got %+v", result.Content[0])
	}
}

// staticManager always returns the same conn for the configured serverName.
type staticManager struct {
	conn       host.ServerConn
	serverName string
}

func (m *staticManager) GetConn(name string) (host.ServerConn, bool) {
	if name == m.serverName {
		return m.conn, true
	}
	return nil, false
}
