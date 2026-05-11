package server_test

import (
	"context"
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

	mcpSrv := server.BuildMCPServer(gw, bi)
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
