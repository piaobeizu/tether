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

// TestOriginGuard_Port443OmitsDefaultPort pins tether#18: on a :443 deployment the
// browser omits the default HTTPS port, so a same-origin POST sends Origin
// "https://<host>" (no ":443"). That must be allowed — otherwise login and wt-ticket
// (every WebTransport connection) 403 and the app is unusable. A non-default port
// must still require the explicit ":<port>", and cross-origin is always rejected.
func TestOriginGuard_Port443OmitsDefaultPort(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	post := func(port int, origin string) int {
		req := httptest.NewRequest("POST", "/api/v1/auth/verify", strings.NewReader("{}"))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		server.WithOriginGuard(port, ok).ServeHTTP(rr, req)
		return rr.Code
	}

	// :443 — both the port-omitted and explicit-:443 forms must pass.
	if code := post(443, "https://127.0.0.1"); code == http.StatusForbidden {
		t.Errorf("port 443, Origin \"https://127.0.0.1\" (no :443): expected pass, got 403")
	}
	if code := post(443, "https://127.0.0.1:443"); code == http.StatusForbidden {
		t.Errorf("port 443, Origin \"https://127.0.0.1:443\": expected pass, got 403")
	}
	// :443 — cross-origin still rejected.
	if code := post(443, "https://evil.example.com"); code != http.StatusForbidden {
		t.Errorf("port 443, cross-origin: expected 403, got %d", code)
	}
	// :443 — http scheme is never same-origin for an https-only daemon.
	if code := post(443, "http://127.0.0.1"); code != http.StatusForbidden {
		t.Errorf("port 443, http scheme: expected 403, got %d", code)
	}
	// :443 — host-suffix lookalike must not match (comparison is exact host equality).
	if code := post(443, "https://127.0.0.1.evil.com"); code != http.StatusForbidden {
		t.Errorf("port 443, host-suffix lookalike: expected 403, got %d", code)
	}
	// Non-default port — the bare (port-omitted) origin must NOT be accepted; explicit must.
	if code := post(8898, "https://127.0.0.1"); code != http.StatusForbidden {
		t.Errorf("port 8898, Origin without port: expected 403 (no default-port leniency), got %d", code)
	}
	if code := post(8898, "https://127.0.0.1:8898"); code == http.StatusForbidden {
		t.Errorf("port 8898, Origin \"https://127.0.0.1:8898\": expected pass, got 403")
	}
}
