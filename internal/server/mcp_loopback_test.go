package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	"github.com/piaobeizu/tether/internal/mcp/host"
	"github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/permission"
	"github.com/piaobeizu/tether/internal/server"
)

func TestMCPLoopback_ToolsListAndCall(t *testing.T) {
	root := t.TempDir()
	bi, err := builtin.New(root)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	mgr := host.NewManager(reg, host.NoopLogger())
	perm := permission.New()
	gw := gateway.New(mgr, reg, perm, host.NoopLogger())

	const port = 19899
	const token = "loopbacktesttoken"

	mcpSrv := server.BuildMCPServer(gw, bi, reg)
	loop := server.NewMCPLoopback(port, mcpSrv, token)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := loop.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		sCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer sCancel()
		_ = loop.Stop(sCtx)
	}()

	time.Sleep(30 * time.Millisecond)

	// Connect via go-sdk HTTP client transport with bearer token.
	client := mcp.NewClient(&mcp.Implementation{Name: "testclient"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		HTTPClient: &http.Client{
			Transport: &bearerTransport{token: token, base: http.DefaultTransport},
		},
		// DisableStandaloneSSE avoids a persistent GET which would also need the
		// bearer token and makes the test simpler.
		DisableStandaloneSSE: true,
	}

	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer sess.Close()

	result, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	found := false
	for _, tool := range result.Tools {
		if tool.Name == "workspace_read_file" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("workspace_read_file not in tools/list; got %v", result.Tools)
	}

	// Request without token must return 401.
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/mcp", port), "application/json", nil)
	if err != nil {
		t.Fatalf("raw POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 without token, got %d", resp.StatusCode)
	}
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (bt *bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+bt.token)
	return bt.base.RoundTrip(r)
}

func TestBuildMCPServer_ObservesRegistryChanges(t *testing.T) {
	reg := registry.New()
	gw := gateway.New(stubMgr{}, reg, alwaysAllow{}, host.NoopLogger())

	noInput := json.RawMessage(`{"type":"object"}`)
	makeTool := func(name string) mcp.Tool {
		return mcp.Tool{Name: name, InputSchema: noInput}
	}

	cfg := host.ServerConfig{Name: "svc"}
	if err := reg.Register(cfg, []mcp.Tool{makeTool("alpha"), makeTool("beta")}); err != nil {
		t.Fatal(err)
	}

	bi, err := builtin.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	srv := server.BuildMCPServer(gw, bi, reg)

	clientT, serverT := mcp.NewInMemoryTransports()
	go func() { _ = srv.Run(t.Context(), serverT) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	session, err := client.Connect(t.Context(), clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	got, err := session.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if !containsTool(got.Tools, "svc_alpha") || !containsTool(got.Tools, "svc_beta") {
		t.Fatalf("expected svc_alpha + svc_beta initially, got %v", toolNames(got.Tools))
	}

	reg.Deregister("svc")
	if err := reg.Register(cfg, []mcp.Tool{makeTool("gamma"), makeTool("delta")}); err != nil {
		t.Fatal(err)
	}

	got, err = session.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if containsTool(got.Tools, "svc_alpha") || containsTool(got.Tools, "svc_beta") {
		t.Fatalf("alpha/beta must be removed after reconnect, got %v", toolNames(got.Tools))
	}
	if !containsTool(got.Tools, "svc_gamma") || !containsTool(got.Tools, "svc_delta") {
		t.Fatalf("expected svc_gamma + svc_delta after reconnect, got %v", toolNames(got.Tools))
	}
}

// TestBuildMCPServer_ObserverFanOutIsBatchedNotPerTool pins the cost *shape* of
// the registry -> BuildMCPServer observer hop: a server that swaps n tools must
// cost the observer a fixed number of fan-out passes and deliver each tool
// exactly once, so total work is Theta(n) and not Theta(n^2).
//
// This replaces an absolute wall-clock assertion ("100 add + 100 remove under
// 100ms", tether#160). That budget measured the machine, not the code: on this
// unchanged code the same batch was measured at 21.7ms-44.0ms under load (a 2x
// spread run-to-run, 2.3x-4.6x from the limit), so a real 2.5x regression would
// still be green on an idle box and the existing code would go red on a busy
// one. An assertion whose red and green do not partition the states you care
// about is not a gate, it is a coin flip that costs a debugging session.
//
// What is asserted instead, at two sizes an order of magnitude apart so the
// numbers below have to be independent of n:
//
//   - passes == 2. One fan-out for the Register, one for the Deregister.
//     If notify() were ever moved inside registry's per-tool loop this is
//     n+n: 200 at n=100, 2000 at n=1000.
//   - each pass carries the whole batch (Added==n on the first, Removed==n on
//     the second). This is the precondition that lets BuildMCPServer's observer
//     collapse a teardown into one srv.RemoveTools(names...) call instead of n;
//     if the event ever arrived per-tool, Removed==1 here.
//   - delivered == 2n exactly. If an event ever carried accumulated state
//     rather than the delta, this is n(n+1) - 10100 at n=100 and 1001000 at
//     n=1000 - which is the quadratic case in its purest form.
//
// BuildMCPServer's own observer is installed and left in place on purpose: the
// production translation really does run over 1000 tools here, so a panic or a
// pathological blowup inside it still surfaces. Its internal srv.AddTool /
// srv.RemoveTools call count is not directly observable from outside the SDK
// (the SDK debounces list_changed notifications over a 10ms window, which
// collapses n calls and 1 call into the same observable), so the event shape
// above is the strongest time-free proxy available at this seam.
func TestBuildMCPServer_ObserverFanOutIsBatchedNotPerTool(t *testing.T) {
	for _, n := range []int{100, 1000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			reg := registry.New()
			gw := gateway.New(stubMgr{}, reg, alwaysAllow{}, host.NoopLogger())
			bi, err := builtin.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			// Installs the production observer; keep it subscribed so the
			// measured fan-out is the real one, not a bare registry.
			_ = server.BuildMCPServer(gw, bi, reg)

			// Second observer, ours, counting what every observer is handed.
			// Registered after the production one, so it also proves the
			// production observer did not swallow or reorder the batch.
			type pass struct{ added, removed int }
			var passes []pass
			reg.AddObserver(func(e registry.RegistryEvent) {
				passes = append(passes, pass{len(e.Added), len(e.Removed)})
			})

			noInput := json.RawMessage(`{"type":"object"}`)
			cfg := host.ServerConfig{Name: "bulk"}
			toolList := make([]mcp.Tool, n)
			for i := range toolList {
				toolList[i] = mcp.Tool{Name: fmt.Sprintf("t%04d", i), InputSchema: noInput}
			}

			if err := reg.Register(cfg, toolList); err != nil {
				t.Fatal(err)
			}
			reg.Deregister("bulk")

			if len(passes) != 2 {
				t.Fatalf("fan-out passes = %d, want 2 (one per registry mutation, "+
					"independent of the %d tools); a per-tool notify gives %d",
					len(passes), n, 2*n)
			}
			if passes[0].added != n || passes[0].removed != 0 {
				t.Errorf("register pass = {added:%d removed:%d}, want {added:%d removed:0}: "+
					"the whole batch has to arrive in one event",
					passes[0].added, passes[0].removed, n)
			}
			if passes[1].removed != n || passes[1].added != 0 {
				t.Errorf("deregister pass = {added:%d removed:%d}, want {added:0 removed:%d}: "+
					"one batched srv.RemoveTools(names...) depends on this",
					passes[1].added, passes[1].removed, n)
			}
			delivered := 0
			for _, p := range passes {
				delivered += p.added + p.removed
			}
			if delivered != 2*n {
				t.Errorf("entries delivered to the observer = %d, want exactly %d "+
					"(%d adds + %d removes, each reported once); re-delivering "+
					"accumulated state would give %d",
					delivered, 2*n, n, n, n*(n+1))
			}
		})
	}
}
