package host_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
)

// stdioServerEnvMgr, when set to "1", makes TestStdioMCPServerHelperMgr act as a
// real stdio MCP server (used as a spawnable child process by the manager tests).
const stdioServerEnvMgr = "TETHER_TEST_MCP_STDIO_MGR"

// TestStdioMCPServerHelperMgr is not a real test: when invoked via re-exec with
// stdioServerEnvMgr=1 it serves a minimal MCP server over stdin/stdout so the
// Manager's synchronous connect succeeds without a fragile fake binary.
func TestStdioMCPServerHelperMgr(t *testing.T) {
	if os.Getenv(stdioServerEnvMgr) != "1" {
		t.Skip("helper process only")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-stdio-mgr", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "noop", Description: "no-op"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

// stdioCmd returns argv that re-execs the test binary as a stdio MCP server.
func stdioCmd() []string {
	return []string{os.Args[0], "-test.run=^TestStdioMCPServerHelperMgr$"}
}

// stdioEnv returns the env needed to activate the helper server.
func stdioEnv() map[string]string {
	return map[string]string{stdioServerEnvMgr: "1"}
}

// hasServerTool reports whether the registry has any tool for the given server.
// Registry prefixes tools as "<name>_<tool>" (host.NamespacePrefix), so we match
// on ServerName rather than an exact prefixed tool name.
func hasServerTool(reg *registry.Registry, server string) bool {
	for _, e := range reg.ListAll() {
		if e.ServerName == server {
			return true
		}
	}
	return false
}

// TestManagerGenerationGuard_StopThenStartKeepsNewTools reproduces tether#6: a
// fast Stop→Start for the same name must not lose the new generation's tools to
// a stale supervisor's async cleanup.
func TestManagerGenerationGuard_StopThenStartKeepsNewTools(t *testing.T) {
	reg := registry.New()
	mgr := host.NewManager(reg, host.NoopLogger())
	defer mgr.StopAll()

	ctx := context.Background()
	cfg := &host.Config{
		Servers: map[string]host.ServerConfig{
			"ext": {Command: stdioCmd(), Env: stdioEnv()},
		},
	}

	// Initial start: tools registered, conn live.
	if err := mgr.Start(ctx, cfg); err != nil {
		t.Fatalf("initial Start: %v", err)
	}
	if !hasServerTool(reg, "ext") {
		t.Fatal("expected 'ext' tool registered after initial Start")
	}
	if _, ok := mgr.GetConn("ext"); !ok {
		t.Fatal("expected live conn for 'ext' after initial Start")
	}

	// Stop deregisters synchronously. Its returned error is the conn.Close()
	// result, which for a CommandContext-backed child is "signal: killed" once
	// the supervisor context is cancelled — benign here, so we ignore it. What
	// matters is that the tools were deregistered before Close.
	_ = mgr.Stop("ext")
	if hasServerTool(reg, "ext") {
		t.Fatal("expected no 'ext' tool after Stop")
	}

	// Re-start (hibernate→wake): a new generation registers the tool again.
	if err := mgr.Start(ctx, cfg); err != nil {
		t.Fatalf("re-Start: %v", err)
	}
	if !hasServerTool(reg, "ext") {
		t.Fatal("expected 'ext' tool registered after re-Start")
	}

	// Give any stale supervisor cleanup time to run; it must be a no-op.
	time.Sleep(150 * time.Millisecond)

	if !hasServerTool(reg, "ext") {
		t.Fatal("stale supervisor cleanup wiped the new generation's 'ext' tool")
	}
	if _, ok := mgr.GetConn("ext"); !ok {
		t.Fatal("stale supervisor cleanup wiped the new generation's 'ext' conn")
	}
}
