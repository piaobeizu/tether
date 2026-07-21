package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

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

// TestIsExempt_StaticSuffixOnlyOutsideProtected pins tether#16: the static-asset
// suffix exemption must apply to SPA assets served at root, but NEVER to a
// client-controlled trailing segment under a protected namespace (/api, /wt, /mcp).
func TestIsExempt_StaticSuffixOnlyOutsideProtected(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))

	// Genuine static assets served by the catch-all "/" handler.
	exempt := []string{
		"/assets/index-abc123.js",
		"/assets/index-abc123.css",
		"/favicon.ico",
		"/logo.svg",
		"/apple-touch-icon.png",
		"/fonts/Inter.woff2",
		"/manifest.webmanifest",
	}
	// Client-controlled trailing segments under protected namespaces that
	// happen to end in an asset suffix — must NOT slip past the cookie check.
	notExempt := []string{
		"/api/v1/work/items/x.js",
		"/api/v1/work/items/evil.css",
		"/api/v1/sessions/foo.png",
		"/api/v1/skills/bar.svg",
		"/api/v1/work/projects.js",
		// Lock the OTHER protected sub-namespaces too, so the guard is proven
		// to cover the whole privileged surface, not just /api/v1/work.
		"/api/v1/permission/x.svg",
		"/api/v1/tasks/foo.css",
		"/api/v1/mcp/tokens/x.png",
	}

	for _, p := range exempt {
		req := httptest.NewRequest("GET", p, nil)
		if !s.IsExemptForTest(req) {
			t.Errorf("static asset %q should be exempt", p)
		}
	}
	for _, p := range notExempt {
		req := httptest.NewRequest("GET", p, nil)
		if s.IsExemptForTest(req) {
			t.Errorf("protected path %q must NOT be exempt via suffix", p)
		}
	}
}

// TestIsExempt_ProdExemptsOnlyRealDistFiles pins tether#17: in production
// (staticFS set) a static-asset path is exempt ONLY if it resolves to a real
// embedded dist file. This is fail-closed against a future non-/api privileged
// prefix (e.g. /admin/x.js) that a mere suffix match would have exempted.
func TestIsExempt_ProdExemptsOnlyRealDistFiles(t *testing.T) {
	dist := fstest.MapFS{
		"assets/index-abc.js":  {Data: []byte("x")},
		"assets/index-abc.css": {Data: []byte("x")},
		"icons/icon-192.png":   {Data: []byte("x")},
		"manifest.webmanifest": {Data: []byte("x")},
		"manifest.json":        {Data: []byte("x")},
		"index.html":           {Data: []byte("x")},
	}
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef")).WithStaticFS(dist)

	exempt := []string{
		"/assets/index-abc.js",
		"/assets/index-abc.css",
		"/icons/icon-192.png",
		"/manifest.webmanifest",
		// Behavior delta vs tether#16 (pin it): these real dist files are now
		// exempt where suffix-mode did not list .html/.json. Both are public
		// (the SPA shell is already served unauthenticated at /auth; manifest
		// is PWA metadata), so exempting them leaks nothing.
		"/index.html",
		"/manifest.json",
	}
	notExempt := []string{
		"/api/v1/work/items/x.js", // protected namespace — never exempt
		"/admin/config.js",        // FUTURE non-/api privileged prefix: not a shipped asset → fail-closed
		"/debug/pprof/heap.png",   // ditto
		"/assets/missing.js",      // asset-shaped but not shipped
		"/nope.png",               // root asset-shaped but absent
		"/assets/../secret.js",    // traversal — fs.ValidPath rejects
		"//api/v1/work/queue.js",  // double slash: residual leading slash after trim → ValidPath rejects
		"/assets",                 // directory — fs.Stat IsDir
		"/assets/",                // directory w/ trailing slash — ValidPath rejects
	}
	for _, p := range exempt {
		if !s.IsExemptForTest(httptest.NewRequest("GET", p, nil)) {
			t.Errorf("prod: %q should be exempt (real dist file)", p)
		}
	}
	for _, p := range notExempt {
		if s.IsExemptForTest(httptest.NewRequest("GET", p, nil)) {
			t.Errorf("prod: %q must NOT be exempt", p)
		}
	}
}

// TestIsExempt_PermissionRequestLoopbackOnly pins tether#39: the PreToolUse
// permission hook POSTs /api/v1/permission/request from loopback with no cookie,
// so it must be exempt for a loopback caller but NOT for a remote one; and the
// sibling /decide must never be exempt (only the authenticated browser approves).
func TestIsExempt_PermissionRequestLoopbackOnly(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))

	withAddr := func(p, addr string) *http.Request {
		r := httptest.NewRequest("POST", p, nil)
		r.RemoteAddr = addr
		return r
	}

	// Loopback hook (IPv4 and IPv6) → exempt.
	if !s.IsExemptForTest(withAddr("/api/v1/permission/request", "127.0.0.1:54321")) {
		t.Error("loopback /api/v1/permission/request should be exempt (the hook carries no cookie)")
	}
	if !s.IsExemptForTest(withAddr("/api/v1/permission/request", "[::1]:54321")) {
		t.Error("IPv6 loopback ::1 /api/v1/permission/request should be exempt")
	}
	// Same path from a remote peer → NOT exempt (falls through to 401).
	if s.IsExemptForTest(withAddr("/api/v1/permission/request", "203.0.113.7:44444")) {
		t.Error("remote /api/v1/permission/request must NOT be exempt")
	}
	// /decide (approval) must stay cookie-gated even from loopback — only the
	// authenticated browser may approve a tool.
	if s.IsExemptForTest(withAddr("/api/v1/permission/abc123/decide", "127.0.0.1:54321")) {
		t.Error("/decide must never be exempt (approval requires the browser session)")
	}
}

// TestMiddleware_APISuffixLookAlikeNotExempt is the end-to-end guard for tether#16:
// an unauthenticated /api path whose trailing segment ends in ".js" must get 401
// and must never reach the inner handler.
func TestMiddleware_APISuffixLookAlikeNotExempt(t *testing.T) {
	s := auth.NewState("tok", []byte("0123456789abcdef0123456789abcdef"))
	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		innerCalled = true
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/work/items/x.js", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET /api/v1/work/items/x.js: expected 401, got %d", rr.Code)
	}
	if innerCalled {
		t.Fatal("inner handler must NOT be reached for a .js-suffixed /api path without a cookie")
	}
}
