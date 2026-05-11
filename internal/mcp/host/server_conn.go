// internal/mcp/host/server_conn.go
package host

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerConn abstracts a live connection to an MCP server.
// The liveConn implementation wraps *mcp.ClientSession.
// Tests use fake implementations.
type ServerConn interface {
	ListTools(ctx context.Context) ([]mcp.Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error)
	Wait() error  // blocks until session closes
	Close() error
}

type liveConn struct {
	session   *mcp.ClientSession
	closeOnce sync.Once
	closeErr  error
}

// NewLiveConn wraps a *mcp.ClientSession as a ServerConn.
// Exported so gateway integration tests can build one from an in-process session.
func NewLiveConn(session *mcp.ClientSession) ServerConn {
	return &liveConn{session: session}
}

func (c *liveConn) ListTools(ctx context.Context) ([]mcp.Tool, error) {
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp/host: ListTools: %w", err)
	}
	out := make([]mcp.Tool, len(result.Tools))
	for i, t := range result.Tools {
		out[i] = *t
	}
	return out, nil
}

func (c *liveConn) CallTool(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("mcp/host: CallTool %s: %w", name, err)
	}
	return result, nil
}

func (c *liveConn) Wait() error { return c.session.Wait() }
func (c *liveConn) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.session.Close() })
	return c.closeErr
}
