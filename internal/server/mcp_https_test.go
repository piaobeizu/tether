package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/mcp/builtin"
	"github.com/piaobeizu/tether/internal/mcp/gateway"
	mcphost "github.com/piaobeizu/tether/internal/mcp/host"
	mcpreg "github.com/piaobeizu/tether/internal/mcp/registry"
	"github.com/piaobeizu/tether/internal/server"
)

// stubMgr / alwaysAllow / containsTool / toolNames are in testhelpers_test.go

func buildTestMCPServer(t *testing.T) (*mcp.Server, *apitoken.Store) {
	t.Helper()
	reg := mcpreg.New()
	gw := gateway.New(stubMgr{}, reg, alwaysAllow{}, mcphost.NoopLogger())
	bi, err := builtin.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := server.BuildMCPServer(gw, bi, reg)

	store, err := apitoken.Open(filepath.Join(t.TempDir(), "api-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return srv, store
}

func TestMCPHTTPS_NoAuthHeader_401(t *testing.T) {
	srv, store := buildTestMCPServer(t)
	h := server.MCPHTTPSHandler(srv, store, nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestMCPHTTPS_BadBearer_401(t *testing.T) {
	srv, store := buildTestMCPServer(t)
	h := server.MCPHTTPSHandler(srv, store, nil)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestMCPHTTPS_ValidBearer_ReachesSDKHandler(t *testing.T) {
	srv, store := buildTestMCPServer(t)
	raw, _, err := store.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	h := server.MCPHTTPSHandler(srv, store, nil)

	srvHTTP := httptest.NewServer(h)
	defer srvHTTP.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "ext"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint: srvHTTP.URL,
		HTTPClient: &http.Client{
			Transport: bearerInjector{token: raw, base: http.DefaultTransport},
		},
		DisableStandaloneSSE: true,
	}
	session, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	got, err := session.ListTools(t.Context(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	found := false
	for _, tl := range got.Tools {
		if tl.Name == "workspace_list_files" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("workspace_list_files missing from tools/list; got %v", toolNames(got.Tools))
	}
}

type bearerInjector struct {
	token string
	base  http.RoundTripper
}

func (b bearerInjector) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func TestMCPTokens_CreateRequiresName(t *testing.T) {
	_, store := buildTestMCPServer(t)
	mux := http.NewServeMux()
	server.RegisterMCPTokensAPI(mux, store, nil)

	req := httptest.NewRequest("POST", "/api/v1/mcp/tokens", strings.NewReader(`{"name":""}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty name, got %d", rr.Code)
	}
}

func TestMCPTokens_CreateReturnsTokenOnce(t *testing.T) {
	_, store := buildTestMCPServer(t)
	mux := http.NewServeMux()
	server.RegisterMCPTokensAPI(mux, store, nil)

	body := strings.NewReader(`{"name":"laptop"}`)
	req := httptest.NewRequest("POST", "/api/v1/mcp/tokens", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Token     string `json:"token"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Token) != 64 || resp.Name != "laptop" || resp.ID == "" {
		t.Fatalf("response shape wrong: %+v", resp)
	}

	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest("GET", "/api/v1/mcp/tokens", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("list: %d", rr2.Code)
	}
	listBody := rr2.Body.String()
	if strings.Contains(listBody, resp.Token) {
		t.Fatal("list response leaked raw token")
	}
	if strings.Contains(listBody, "hash") {
		t.Fatal("list response leaked hash field")
	}
}

func TestMCPTokens_Delete_204Then404(t *testing.T) {
	_, store := buildTestMCPServer(t)
	raw, tok, _ := store.Create("name")
	mux := http.NewServeMux()
	server.RegisterMCPTokensAPI(mux, store, nil)

	req := httptest.NewRequest("DELETE", "/api/v1/mcp/tokens/"+tok.ID, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if store.Validate(raw) {
		t.Fatal("revoked token must fail Validate")
	}

	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, httptest.NewRequest("DELETE", "/api/v1/mcp/tokens/"+tok.ID, nil))
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", rr2.Code)
	}
}

func TestMCPHTTPS_AuditLogsOmitSecrets(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	srv, store := buildTestMCPServer(t)
	raw, tok, _ := store.Create("laptop")

	expectedHash := sha256Hex(raw)

	h := server.MCPHTTPSHandler(srv, store, logger)
	srvHTTP := httptest.NewServer(h)
	defer srvHTTP.Close()

	req, _ := http.NewRequest("POST", srvHTTP.URL+"/mcp", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	_, _ = http.DefaultClient.Do(req)

	req2, _ := http.NewRequest("POST", srvHTTP.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/list","id":1}`))
	req2.Header.Set("Authorization", "Bearer "+raw)
	_, _ = http.DefaultClient.Do(req2)

	logs := buf.String()
	if strings.Contains(logs, raw) {
		t.Fatalf("audit log leaked raw token: %s", logs)
	}
	if strings.Contains(logs, expectedHash) {
		t.Fatalf("audit log leaked token hash: %s", logs)
	}
	if !strings.Contains(logs, "mcp.apitoken.auth_failed") {
		t.Fatal("missing auth_failed event")
	}
	if !strings.Contains(logs, "mcp.apitoken.auth_ok") {
		t.Fatal("missing auth_ok event")
	}
	_ = tok
}

func sha256Hex(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
