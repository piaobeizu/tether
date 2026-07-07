package auth_test

import (
	"encoding/json"
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

func TestMiddleware_WTPaths_Exempt(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s := auth.NewState("tok", secret)
	innerCalled := 0
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled++
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/wt/chat", "/wt/events", "/wt/shell", "/wt/_smoke", "/wt/control"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", p, nil)
		h.ServeHTTP(rr, req)
		// Middleware passes without cookie; inner handler decides auth via ticket.
		if rr.Code != http.StatusOK {
			t.Fatalf("path %q: expected 200 (exempt from cookie check), got %d", p, rr.Code)
		}
	}
	if innerCalled != 5 {
		t.Fatalf("inner handler hit %d times, want 5", innerCalled)
	}
}

func TestWtTicketHandler_IssuesTicket(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s := auth.NewState("tok", secret)

	// Mint a session JWT so the handler has a cookie to read.
	sessionJWT, err := auth.IssueJWT(secret, "client-abc")
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/auth/wt-ticket", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sessionJWT})
	s.WtTicketHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal("decode response:", err)
	}
	ticket, ok := body["ticket"]
	if !ok || ticket == "" {
		t.Fatal("response missing 'ticket' field")
	}

	// Ticket must verify with VerifyWTTicket.
	clientID, valid := auth.VerifyWTTicket(secret, ticket)
	if !valid {
		t.Fatal("issued ticket does not verify")
	}
	if clientID != "client-abc" {
		t.Fatalf("clientID: got %q, want %q", clientID, "client-abc")
	}
}

func TestClientIDFromTicket_ValidAndInvalid(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s := auth.NewState("tok", secret)

	ticket, err := auth.IssueWTTicket(secret, "client-xyz")
	if err != nil {
		t.Fatal(err)
	}

	// Valid ticket.
	req := httptest.NewRequest("GET", "/wt/chat?ticket="+ticket, nil)
	if got := s.ClientIDFromTicket(req); got != "client-xyz" {
		t.Fatalf("got %q, want client-xyz", got)
	}

	// Missing ticket.
	req2 := httptest.NewRequest("GET", "/wt/chat", nil)
	if got := s.ClientIDFromTicket(req2); got != "" {
		t.Fatalf("missing ticket: got %q, want empty", got)
	}

	// Session JWT must NOT be accepted as a WT ticket.
	sessionJWT, _ := auth.IssueJWT(secret, "client-xyz")
	req3 := httptest.NewRequest("GET", "/wt/chat?ticket="+sessionJWT, nil)
	if got := s.ClientIDFromTicket(req3); got != "" {
		t.Fatalf("session JWT accepted as wt-ticket (must be rejected); got %q", got)
	}
}

func TestIsExempt_OAuthAndWellKnownExact(t *testing.T) {
	s := auth.NewState("tok", []byte("secret01secret02secret03secret04"))

	exempt := []string{
		"/oauth/authorize",
		"/oauth/token",
		"/.well-known/oauth-authorization-server",
	}
	notExempt := []string{
		"/.well-known/other",
		"/.well-known/",
		"/oauth/",
		"/api/v1/mcp/tokens",
	}

	for _, p := range exempt {
		req := httptest.NewRequest("GET", p, nil)
		if !s.IsExemptForTest(req) {
			t.Errorf("path %q should be exempt", p)
		}
	}
	for _, p := range notExempt {
		req := httptest.NewRequest("GET", p, nil)
		if s.IsExemptForTest(req) {
			t.Errorf("path %q should NOT be exempt", p)
		}
	}
}
