package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
)

// MCPLoopback serves the MCP Streamable HTTP endpoint on a plain HTTP loopback
// listener. All requests must carry Authorization: Bearer <token>.
type MCPLoopback struct {
	srv     *http.Server
	token   string
	handler http.Handler
}

// BuildMCPServer constructs the singleton *mcp.Server from gateway proxies and
// builtins. Call once at daemon startup; all CC sessions share the same server
// instance.
func BuildMCPServer(gw *gateway.Gateway, bi *builtin.Registry) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "tether", Version: "v0.3.1"}, nil)
	for _, e := range gw.ListToolsSnapshot() {
		entry := e // capture for closure
		srv.AddTool(&entry.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return gw.CallTool(ctx, gateway.CallRequest{
				ToolName:  entry.PrefixedName,
				Arguments: req.Params.Arguments,
			})
		})
	}
	bi.RegisterInto(srv)
	return srv
}

// NewMCPLoopback creates (but does not start) a loopback HTTP server that
// exposes mcpSrv at /mcp on 127.0.0.1:<port>.
func NewMCPLoopback(port int, mcpSrv *mcp.Server, token string) *MCPLoopback {
	sdkHandler := mcp.NewStreamableHTTPHandler(
		func(_ *http.Request) *mcp.Server { return mcpSrv },
		&mcp.StreamableHTTPOptions{
			SessionTimeout: 30 * time.Minute,
		},
	)
	l := &MCPLoopback{token: token, handler: sdkHandler}
	mux := http.NewServeMux()
	mux.Handle("/mcp", l)
	mux.Handle("/mcp/", l)
	l.srv = &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: mux,
	}
	return l
}

// ServeHTTP validates the bearer token before handing off to the SDK handler.
func (l *MCPLoopback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+l.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	l.handler.ServeHTTP(w, r)
}

// Start begins listening on 127.0.0.1:<port>. It returns once the listener is
// bound so callers can immediately connect.
func (l *MCPLoopback) Start(_ context.Context) error {
	ln, err := net.Listen("tcp", l.srv.Addr)
	if err != nil {
		return fmt.Errorf("mcp loopback: listen %s: %w", l.srv.Addr, err)
	}
	go func() {
		if err := l.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp loopback: serve error", "err", err)
		}
	}()
	return nil
}

// Stop drains active sessions within the deadline.
func (l *MCPLoopback) Stop(ctx context.Context) error {
	return l.srv.Shutdown(ctx)
}
