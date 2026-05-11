package server_test

import (
	"context"

	mcphost "github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/permission"
)

type stubMgr struct{}

func (stubMgr) GetConn(_ string) (mcphost.ServerConn, bool) { return nil, false }

type alwaysAllow struct{}

func (alwaysAllow) Check(_ context.Context, _ *permission.Request) (*permission.Decision, error) {
	return &permission.Decision{Allow: true}, nil
}

func containsTool(ts []*mcp.Tool, name string) bool {
	for _, t := range ts {
		if t.Name == name {
			return true
		}
	}
	return false
}

func toolNames(ts []*mcp.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}
