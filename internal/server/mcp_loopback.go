package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	mcpreg "github.com/piaobeizu/tether/internal/mcp/registry"
)

// MCPLoopback serves the MCP Streamable HTTP endpoint on a plain HTTP loopback
// listener. All requests must carry Authorization: Bearer <token>.
type MCPLoopback struct {
	srv     *http.Server
	token   string
	handler http.Handler
}

// mcpTokenFile is the name, inside ~/.tether, of the file Run drops the
// per-daemon bearer token into. One constant because two packages now care:
// Run writes it, and `tether doctor` reads it back to prove that whatever
// holds the MCP port answers to this deployment's secret (tether#89).
const mcpTokenFile = "mcp-token"

// MCPTokenPath names ~/.tether/mcp-token without creating anything.
//
// Split out for doctor, in the same shape and for the same reason as
// tetherDataDirPath: a diagnostic that creates what it is about to report on
// turns a real finding into a pass on the second run.
//
// The token is regenerated on every start (generateBearerToken in Run) and
// written before the settings.json injection, so it is written even under
// --skip-mcp-inject — which is why it, and not the Authorization header in cc's
// settings.json, is the primary evidence doctor uses. Two caveats doctor has to
// live with, and does: the write is best-effort (Run warns and carries on), and
// a graceful shutdown removes the file again, so its absence is an ordinary
// state and never a verdict on anything.
func MCPTokenPath() (string, error) {
	dir, err := tetherDataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, mcpTokenFile), nil
}

// BuildMCPServer constructs the singleton *mcp.Server from gateway proxies and
// builtins. The server subscribes to registry changes so that supervisor
// reconnects keep the tool list in sync without requiring clients to reconnect.
//
// Thread-safety: go-sdk Server.AddTool and Server.RemoveTools both route
// through changeAndNotify which acquires the Server's internal mutex before
// mutating tool state. The SDK automatically dispatches
// notifications/tools/list_changed to every connected session.
func BuildMCPServer(gw *gateway.Gateway, bi *builtin.Registry, reg *mcpreg.Registry) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "tether", Version: "v0.3.2"}, nil)

	addOne := func(entry mcpreg.ToolEntry) {
		// Use a copy with the prefixed name so the tool appears with its
		// namespaced name in tools/list (e.g. "svc_alpha" not "alpha").
		t := entry.Tool
		t.Name = entry.PrefixedName
		srv.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return gw.CallTool(ctx, gateway.CallRequest{
				ToolName:  entry.PrefixedName,
				Arguments: req.Params.Arguments,
			})
		})
	}

	for _, e := range gw.ListToolsSnapshot() {
		addOne(e)
	}
	bi.RegisterInto(srv)

	reg.AddObserver(func(e mcpreg.RegistryEvent) {
		if len(e.Removed) > 0 {
			names := make([]string, len(e.Removed))
			for i, t := range e.Removed {
				names[i] = t.PrefixedName
			}
			srv.RemoveTools(names...)
		}
		for _, entry := range e.Added {
			addOne(entry)
		}
	})

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
// Uses constant-time comparison to avoid timing side-channels.
func (l *MCPLoopback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Authorization")
	want := "Bearer " + l.token
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	l.handler.ServeHTTP(w, r)
}

// Start begins listening on 127.0.0.1:<port>. It returns once the listener is
// bound so callers can immediately connect.
func (l *MCPLoopback) Start() error {
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
