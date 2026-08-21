package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
)

// TestIsExempt_OAuthAuthorize_GETOnly pins tether#117 A1 at the auth boundary.
//
// /oauth/authorize used to be exempt for EVERY method, so the POST that MINTS an
// authorization code was reachable with zero credentials. On a --acme-domain
// deployment (TCP+UDP both on *:443, reachable from the public internet because
// TLS-ALPN-01 requires it) three curl requests turned that code into a 24h
// Bearer token in the same api-tokens store as a hand-issued one.
//
// The consent POST now needs the browser session. GET stays exempt (it only
// renders the approval page) and POST /oauth/token stays exempt (the code +
// PKCE verifier authenticate that exchange on their own, and the MCP client
// that performs it has no cookie jar).
func TestIsExempt_OAuthAuthorize_GETOnly(t *testing.T) {
	s := auth.NewState("tok", []byte("secret01secret02secret03secret04"))

	if !s.IsExemptForTest(httptest.NewRequest("GET", "/oauth/authorize", nil)) {
		t.Error("GET /oauth/authorize should stay exempt — it only renders the consent page")
	}
	if s.IsExemptForTest(httptest.NewRequest("POST", "/oauth/authorize", nil)) {
		t.Error("POST /oauth/authorize must NOT be exempt: consent must come from an authenticated browser")
	}
	if s.IsExemptForTest(httptest.NewRequest("PUT", "/oauth/authorize", nil)) {
		t.Error("PUT /oauth/authorize must NOT be exempt")
	}
	// Unchanged by A1 — pinned here so a future edit to the same switch cannot
	// break the exchange without a test noticing.
	if !s.IsExemptForTest(httptest.NewRequest("POST", "/oauth/token", nil)) {
		t.Error("POST /oauth/token must stay exempt (code + PKCE self-authenticate)")
	}
	if !s.IsExemptForTest(httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)) {
		t.Error("the RFC 8414 metadata document must stay exempt")
	}
}

// TestMiddleware_OAuthAuthorizePost_NoCookie_NeverReachesHandler is the
// end-to-end form: without a session cookie the consent POST must not reach the
// handler at all, so no authorization code is ever created.
func TestMiddleware_OAuthAuthorizePost_NoCookie_NeverReachesHandler(t *testing.T) {
	s := auth.NewState("tok", testSecret)
	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/authorize",
		strings.NewReader("req_id=whatever&action=allow"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, req)

	if innerCalled {
		t.Fatal("unauthenticated POST /oauth/authorize reached the code-minting handler")
	}
	if rr.Code == http.StatusOK || rr.Code == http.StatusFound && rr.Header().Get("Location") != "/auth" {
		t.Fatalf("unauthenticated consent POST: code=%d location=%q; want a redirect to /auth or a 401",
			rr.Code, rr.Header().Get("Location"))
	}
}

// TestMiddleware_OAuthAuthorizePost_WithCookie_ReachesHandler is the other half:
// the fix must not break the real consent flow from a signed-in browser.
func TestMiddleware_OAuthAuthorizePost_WithCookie_ReachesHandler(t *testing.T) {
	s := auth.NewState("tok", testSecret)
	tok, err := auth.IssueJWT(testSecret, "client-abc")
	if err != nil {
		t.Fatal(err)
	}
	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/authorize",
		strings.NewReader("req_id=whatever&action=allow"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	h.ServeHTTP(rr, req)

	if !innerCalled || rr.Code != http.StatusOK {
		t.Fatalf("authenticated consent POST: innerCalled=%v code=%d, want true/200", innerCalled, rr.Code)
	}
}
