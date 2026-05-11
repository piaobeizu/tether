// internal/mcp/gateway/gateway.go
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/permission"
)

// PermissionChecker checks whether a tool call is allowed.
// Satisfied by *permission.Manager and test stubs.
type PermissionChecker interface {
	Check(ctx context.Context, req *permission.Request) (*permission.Decision, error)
}

// ManagerReader provides read access to live server connections.
// Note: AllTools is intentionally absent — gateway uses reg.ListAll() directly.
type ManagerReader interface {
	GetConn(name string) (host.ServerConn, bool)
}

// CallRequest is the input to Gateway.CallTool.
type CallRequest struct {
	SessionID string
	ToolName  string         // prefixed name as seen by caller
	Arguments map[string]any
}

// Gateway is the single entry point for MCP tool calls.
type Gateway struct {
	mgr     ManagerReader
	reg     *registry.Registry
	perm    PermissionChecker
	auditor *auditor
}

// New creates a Gateway.
func New(mgr ManagerReader, reg *registry.Registry, perm PermissionChecker, logger host.HistoryLogger) *Gateway {
	return &Gateway{
		mgr:     mgr,
		reg:     reg,
		perm:    perm,
		auditor: &auditor{logger: logger},
	}
}

// ListTools returns all currently registered tools with prefixed names.
func (g *Gateway) ListTools(_ context.Context, _ string) ([]mcp.Tool, error) {
	entries := g.reg.ListAll()
	tools := make([]mcp.Tool, len(entries))
	for i, e := range entries {
		t := e.Tool
		t.Name = e.PrefixedName
		tools[i] = t
	}
	return tools, nil
}

// CallTool runs the permission check and dispatches to the correct server.
// Tool-domain failures (permission denied, tool not found, server down) return
// CallToolResult{IsError: true} with nil error so the LLM can read the message.
// Internal failures (perm.Check error) return a non-nil error.
func (g *Gateway) CallTool(ctx context.Context, req CallRequest) (*mcp.CallToolResult, error) {
	serverName, origName, ok := g.reg.Lookup(req.ToolName)
	if !ok {
		return errorResult(fmt.Sprintf("tool not found: %s", req.ToolName)), nil
	}

	decision, err := g.perm.Check(ctx, &permission.Request{
		Source:    "mcp:" + serverName,
		SessionID: req.SessionID,
		ToolName:  req.ToolName,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp/gateway: permission check: %w", err)
	}
	if !decision.Allow {
		g.auditor.toolCallDenied(req.ToolName, "mcp:"+serverName, decision.Reason)
		return errorResult("permission denied: " + req.ToolName), nil
	}

	callID := newShortID()
	g.auditor.toolCallStarted(callID, req.ToolName, serverName)

	conn, ok := g.mgr.GetConn(serverName)
	if !ok {
		g.auditor.toolCallFailed(callID, "server unavailable")
		return errorResult(fmt.Sprintf("tool %s unavailable (server restarting)", req.ToolName)), nil
	}

	result, err := conn.CallTool(ctx, origName, req.Arguments)
	if err != nil {
		g.auditor.toolCallFailed(callID, err.Error())
		return errorResult(fmt.Sprintf("tool call failed: %v", err)), nil
	}
	g.auditor.toolCallCompleted(callID, len(result.Content))
	return result, nil
}

func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

func newShortID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
