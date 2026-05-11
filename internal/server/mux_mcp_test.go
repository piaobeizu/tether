package server_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/server"
)

func TestTokensAPI_RejectsCrossOriginPost(t *testing.T) {
	store, err := apitoken.Open(filepath.Join(t.TempDir(), "api-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	server.RegisterMCPTokensAPI(mux, store, nil)
	wrapped := server.WithOriginGuard(8898, mux)

	// Cross-origin POST must be 403.
	req := httptest.NewRequest("POST", "/api/v1/mcp/tokens", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST: expected 403, got %d", rr.Code)
	}

	// Origin-absent (curl/programmatic) must pass through.
	req2 := httptest.NewRequest("POST", "/api/v1/mcp/tokens", strings.NewReader(`{"name":"x"}`))
	req2.Header.Set("Content-Type", "application/json")
	rr2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rr2, req2)
	if rr2.Code == http.StatusForbidden {
		t.Fatalf("Origin-absent POST: expected pass-through, got 403")
	}
}
