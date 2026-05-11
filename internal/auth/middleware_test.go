package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
)

func TestMiddleware_MCPPathExempt(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))
	called := 0
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/mcp", "/mcp/foo", "/mcp/some/sub/path"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("path %q: expected 200 (exempt), got %d", p, rr.Code)
		}
	}
	if called != 3 {
		t.Fatalf("inner handler hit %d times, want 3", called)
	}
}

func TestMiddleware_APIv1MCPNotExempt(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))
	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		innerCalled = true
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/mcp/tokens", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth POST /api/v1/mcp/tokens: expected 401, got %d", rr.Code)
	}
	if innerCalled {
		t.Fatal("inner handler must NOT be reached without a valid session cookie")
	}
}

func TestMiddleware_MCPLookAlikeNotExempt(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))
	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		innerCalled = true
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/mcpadmin", nil)
	h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK || innerCalled {
		t.Fatalf("'/mcpadmin' must NOT be exempt (got code=%d, innerCalled=%v)", rr.Code, innerCalled)
	}
}
