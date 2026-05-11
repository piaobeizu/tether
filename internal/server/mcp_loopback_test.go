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

func TestBuildMCPServer_ObserverLatencyBudget(t *testing.T) {
	reg := registry.New()
	gw := gateway.New(stubMgr{}, reg, alwaysAllow{}, host.NoopLogger())
	bi, err := builtin.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = server.BuildMCPServer(gw, bi, reg)

	noInput := json.RawMessage(`{"type":"object"}`)
	cfg := host.ServerConfig{Name: "bulk"}
	toolList := make([]mcp.Tool, 100)
	for i := range toolList {
		toolList[i] = mcp.Tool{Name: fmt.Sprintf("t%03d", i), InputSchema: noInput}
	}

	start := time.Now()
	if err := reg.Register(cfg, toolList); err != nil {
		t.Fatal(err)
	}
	reg.Deregister("bulk")
	dur := time.Since(start)

	if dur > 100*time.Millisecond {
		t.Fatalf("observer batch (100 add + 100 remove) took %v, budget 100ms", dur)
	}
}
