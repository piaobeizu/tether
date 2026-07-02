package instance

// White-box tests for the v0.5 hibernate/wake mechanism (plan steps 4–5).
// Internal package so we can assert on the unexported mgr/reg wiring, and use
// an in-memory MCP client against the instance's own *mcp.Server to verify
// cold-tool retention + lazy wake without HTTP/bearer plumbing.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/permission"
)

const stdioServerEnvInst = "TETHER_TEST_MCP_STDIO_INST"

// TestStdioMCPServerHelperInstance is not a real test: re-exec'd with the env
// set, it serves a minimal stdio MCP server exposing a single "noop" tool so
// the instance's Manager can perform a real MCP connect.
func TestStdioMCPServerHelperInstance(t *testing.T) {
	if os.Getenv(stdioServerEnvInst) != "1" {
		t.Skip("helper process only")
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test-stdio-inst", Version: "v0.0.1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "noop", Description: "no-op"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	_ = srv.Run(context.Background(), &mcp.StdioTransport{})
}

func stdioCmdInst() []string {
	return []string{os.Args[0], "-test.run=^TestStdioMCPServerHelperInstance$"}
}
func stdioEnvInst() map[string]string { return map[string]string{stdioServerEnvInst: "1"} }

// startWithExtServer builds and starts an instance with one external stdio MCP
// server registered under the given name/prefix.
func startWithExtServer(t *testing.T, name, prefix string) *MCPInstance {
	t.Helper()
	inst, err := New(InstanceConfig{
		TaskID:      "t-01HIBERNATE",
		TaskSlug:    "hib-test",
		WsRoot:      t.TempDir(),
		PermManager: permission.New(),
		SkipInject:  true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	servers := map[string]host.ServerConfig{
		name: {Command: stdioCmdInst(), Env: stdioEnvInst(), Prefix: prefix},
	}
	if err := inst.Start(context.Background(), servers); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return inst
}

func stopInst(inst *MCPInstance) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = inst.Stop(ctx)
}

func TestHibernateWake_TogglesChildAndRegistry(t *testing.T) {
	inst := startWithExtServer(t, "ext", "ext")
	defer stopInst(inst)

	// After Start: child connected + external tool in the registry.
	if _, ok := inst.mgr.GetConn("ext"); !ok {
		t.Fatal("expected child conn 'ext' after Start")
	}
	if len(inst.reg.ListAll()) == 0 {
		t.Fatal("expected external tool registered after Start")
	}
	if inst.Dormant() {
		t.Fatal("should not be dormant after Start")
	}

	// Hibernate: child stopped, registry drained, dormant flag set.
	if err := inst.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if !inst.Dormant() {
		t.Fatal("expected Dormant() after Hibernate")
	}
	if _, ok := inst.mgr.GetConn("ext"); ok {
		t.Fatal("child conn should be gone after Hibernate")
	}
	if len(inst.reg.ListAll()) != 0 {
		t.Fatal("registry should be drained after Hibernate")
	}

	// Wake: child restarted, registry repopulated, awake.
	if err := inst.Wake(context.Background()); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if inst.Dormant() {
		t.Fatal("should be awake after Wake")
	}
	if _, ok := inst.mgr.GetConn("ext"); !ok {
		t.Fatal("child conn should be back after Wake")
	}
	if len(inst.reg.ListAll()) == 0 {
		t.Fatal("external tool should be back in registry after Wake")
	}
}

func TestColdToolRetentionAndLazyWake(t *testing.T) {
	// The tool call below hits the gateway permission gate, which has no
	// approver in this unit test; shorten its deadline so the (denied) call
	// returns promptly instead of blocking for the default 60s.
	origTimeout := permission.Timeout
	permission.Timeout = 200 * time.Millisecond
	defer func() { permission.Timeout = origTimeout }()

	inst := startWithExtServer(t, "ext", "ext")
	defer stopInst(inst)

	// Connect an in-memory MCP client to the instance's own server.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = inst.Server().Run(ctx, serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer cs.Close()

	hasExtNoop := func(when string) {
		res, err := cs.ListTools(ctx, &mcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("ListTools (%s): %v", when, err)
		}
		for _, tool := range res.Tools {
			if tool.Name == "ext_noop" {
				return
			}
		}
		t.Fatalf("ext_noop not advertised %s", when)
	}

	// Advertised while running.
	hasExtNoop("while running")

	// Hibernate → tool defs retained on the server (client still sees it), dormant.
	if err := inst.Hibernate(ctx); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if !inst.Dormant() {
		t.Fatal("expected dormant after Hibernate")
	}
	hasExtNoop("after Hibernate (cold retention)")

	// Calling the cold external tool routes through the server's retained tool
	// handler, which triggers lazy Wake *before* reaching the gateway. The call
	// itself is permission-denied (no approver here) — that is orthogonal and
	// covered by gateway tests; what this verifies is that reaching the handler
	// woke the instance (child servers re-spawned, registry repopulated).
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "ext_noop", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool ext_noop transport error: %v", err)
	}
	_ = res // result is permission-gated; not asserted here
	if inst.Dormant() {
		t.Fatal("instance should have woken after the external tool call reached the handler")
	}
}

func TestHibernate_NoopWithoutExternalServers(t *testing.T) {
	inst, err := New(InstanceConfig{
		TaskID: "t-01NOEXT", TaskSlug: "noext", WsRoot: t.TempDir(),
		PermManager: permission.New(), SkipInject: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := inst.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stopInst(inst)

	// No external servers → nothing to reclaim → Hibernate is a no-op.
	if err := inst.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if inst.Dormant() {
		t.Fatal("builtin-only instance should not become dormant")
	}
}

func TestHibernateWake_Idempotent(t *testing.T) {
	inst := startWithExtServer(t, "ext", "ext")
	defer stopInst(inst)

	// Wake when already awake is a no-op.
	if err := inst.Wake(context.Background()); err != nil {
		t.Fatalf("Wake (awake) should be no-op: %v", err)
	}
	if inst.Dormant() {
		t.Fatal("Wake on awake instance must not set dormant")
	}

	// Double Hibernate.
	if err := inst.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate 1: %v", err)
	}
	if err := inst.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate 2 (idempotent): %v", err)
	}
	if !inst.Dormant() {
		t.Fatal("expected dormant after Hibernate")
	}

	// Double Wake.
	if err := inst.Wake(context.Background()); err != nil {
		t.Fatalf("Wake 1: %v", err)
	}
	if err := inst.Wake(context.Background()); err != nil {
		t.Fatalf("Wake 2 (idempotent): %v", err)
	}
	if inst.Dormant() {
		t.Fatal("expected awake after Wake")
	}
}

func TestWakeFailureRollsBackAndStaysDormant(t *testing.T) {
	inst := startWithExtServer(t, "ext", "ext")
	defer stopInst(inst)

	if err := inst.Hibernate(context.Background()); err != nil {
		t.Fatalf("Hibernate: %v", err)
	}
	if !inst.Dormant() {
		t.Fatal("expected dormant after Hibernate")
	}

	// Simulate the external servers becoming unspawnable before wake: one good
	// (re-exec) server plus one whose binary does not exist. Map order is
	// non-deterministic, so whichever one starts first must be rolled back.
	inst.mu.Lock()
	inst.extraServers = map[string]host.ServerConfig{
		"good": {Command: stdioCmdInst(), Env: stdioEnvInst(), Prefix: "good"},
		"bad":  {Command: []string{"/nonexistent-tether-test-binary"}, Prefix: "bad"},
	}
	inst.mu.Unlock()

	if err := inst.Wake(context.Background()); err == nil {
		t.Fatal("expected Wake to fail when a child cannot spawn")
	}
	// Failed Wake must leave the instance dormant (retryable), not stuck awake.
	if !inst.Dormant() {
		t.Fatal("instance must stay dormant after a failed Wake")
	}
	// And it must not leak a partially-started child.
	if _, ok := inst.mgr.GetConn("good"); ok {
		t.Fatal("failed Wake leaked a partially-started 'good' child conn (no rollback)")
	}
	if _, ok := inst.mgr.GetConn("bad"); ok {
		t.Fatal("'bad' child conn should not exist")
	}
}
