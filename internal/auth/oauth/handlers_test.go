package oauth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/auth/oauth"
)

// sessionCookieName is the cookie these tests use to stand in for a signed-in
// browser. The real wiring hands NewHandlers auth.State.ClientIDFromRequest,
// which HMAC-verifies the tether_session JWT; what this package's tests need to
// pin is that the consent POST consults SessionAuth AT ALL and obeys it, not how
// a JWT is verified (internal/auth/jwt_test.go covers that).
const sessionCookieName = "tether_session"

// testSessionAuth accepts a request iff it carries sessionCookieName.
func testSessionAuth(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	return err == nil && c.Value != ""
}

// withSession marks req as coming from a signed-in browser.
func withSession(req *http.Request) *http.Request {
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "signed-in"})
	return req
}

func makeHandlers(t *testing.T) (*oauth.Handlers, *apitoken.Store) {
	t.Helper()
	tokens, err := apitoken.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs := oauth.NewCodeStore()
	h := oauth.NewHandlers(cs, tokens, "https://localhost:8897", testSessionAuth)
	return h, tokens
}

// makeHandlersNoSessionAuth builds Handlers the way a caller that forgot to wire
// SessionAuth would. Kept separate so the nil case is exercised on purpose
// rather than by accident.
func makeHandlersNoSessionAuth(t *testing.T) *oauth.Handlers {
	t.Helper()
	tokens, err := apitoken.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	return oauth.NewHandlers(oauth.NewCodeStore(), tokens, "https://localhost:8897", nil)
}

func pkceTestPair() (verifier, challenge string) {
	verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// --- Metadata ---

func TestMetadata(t *testing.T) {
	h, _ := makeHandlers(t)
	req := httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil)
	w := httptest.NewRecorder()
	h.Metadata().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var m map[string]any
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["issuer"] != "https://localhost:8897" {
		t.Errorf("issuer = %v", m["issuer"])
	}
	methods, _ := m["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("methods = %v", methods)
	}
}

// --- Authorize GET ---

func authorizeURL(challenge string, extra url.Values) string {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {"test-client"},
		"redirect_uri":          {"http://localhost:12345/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	for k, vals := range extra {
		v[k] = vals
	}
	return "/oauth/authorize?" + v.Encode()
}

func TestAuthorizeGet_ValidRequest_ShowsApprovalPage(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	req := httptest.NewRequest("GET", authorizeURL(challenge, nil), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "test-client") {
		t.Error("approval page should display client_id")
	}
	if !strings.Contains(body, `name="req_id"`) {
		t.Error("form should contain req_id hidden input")
	}
}

func TestAuthorizeGet_XSSClientID_IsEscaped(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	xssID := `<script>alert(1)</script>`
	req := httptest.NewRequest("GET", authorizeURL(challenge, url.Values{"client_id": {xssID}}), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("client_id must be HTML-escaped in the approval page")
	}
}

func TestAuthorizeGet_NonLoopbackRedirectURI_Rejects(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	req := httptest.NewRequest("GET", authorizeURL(challenge, url.Values{"redirect_uri": {"https://evil.example.com/steal"}}), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestAuthorizeGet_MissingCodeChallenge_Redirects(t *testing.T) {
	h, _ := makeHandlers(t)
	// No code_challenge but valid redirect_uri → redirect with error
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {"c"},
		"redirect_uri":          {"http://localhost:12345/callback"},
		"code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest("GET", "/oauth/authorize?"+v.Encode(), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "error=invalid_request") {
		t.Errorf("expected error in redirect, got %s", loc)
	}
}

func TestAuthorizeGet_PlainPKCE_Rejects(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	req := httptest.NewRequest("GET", authorizeURL(challenge, url.Values{"code_challenge_method": {"plain"}}), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "error=invalid_request") {
		t.Error("plain PKCE should produce invalid_request redirect")
	}
}

// --- Authorize POST ---

func reqIDFromApprovalPage(t *testing.T, h *oauth.Handlers) string {
	t.Helper()
	_, challenge := pkceTestPair()
	req := httptest.NewRequest("GET", authorizeURL(challenge, nil), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)
	body := w.Body.String()
	start := strings.Index(body, `name="req_id" value="`)
	if start < 0 {
		t.Fatal("req_id not found in approval page")
	}
	start += len(`name="req_id" value="`)
	end := strings.Index(body[start:], `"`)
	return body[start : start+end]
}

func TestAuthorizePost_Allow_IssuesCodeAndRedirects(t *testing.T) {
	h, _ := makeHandlers(t)
	reqID := reqIDFromApprovalPage(t, h)

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://localhost:12345/callback?") {
		t.Errorf("unexpected redirect: %s", loc)
	}
	u, _ := url.Parse(loc)
	if u.Query().Get("code") == "" {
		t.Error("redirect should contain code param")
	}
}

func TestAuthorizePost_Deny_RedirectsWithAccessDenied(t *testing.T) {
	h, _ := makeHandlers(t)
	reqID := reqIDFromApprovalPage(t, h)

	form := url.Values{"req_id": {reqID}, "action": {"deny"}}
	req := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	if !strings.Contains(w.Header().Get("Location"), "error=access_denied") {
		t.Errorf("expected access_denied in redirect, got %s", w.Header().Get("Location"))
	}
}

func TestAuthorizePost_ExpiredReqID_Returns400(t *testing.T) {
	h, _ := makeHandlers(t)
	form := url.Values{"req_id": {"doesnotexist"}, "action": {"allow"}}
	req := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// --- Token endpoint ---

// fullPKCECode performs GET+POST authorize and returns the issued code + verifier.
func fullPKCECode(t *testing.T, h *oauth.Handlers) (code, verifier string) {
	t.Helper()
	verifier, challenge := pkceTestPair()

	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {"cursor"},
		"redirect_uri":          {"http://localhost:12345/callback"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
	}
	req := httptest.NewRequest("GET", "/oauth/authorize?"+v.Encode(), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)
	body := w.Body.String()
	start := strings.Index(body, `name="req_id" value="`)
	start += len(`name="req_id" value="`)
	end := strings.Index(body[start:], `"`)
	reqID := body[start : start+end]

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req2 := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w2, req2)
	loc := w2.Header().Get("Location")
	u, _ := url.Parse(loc)
	return u.Query().Get("code"), verifier
}

func TestToken_HappyPath(t *testing.T) {
	h, tokens := makeHandlers(t)
	code, verifier := fullPKCECode(t, h)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://localhost:12345/callback"},
		"client_id":     {"cursor"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Token().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	raw, _ := resp["access_token"].(string)
	if raw == "" {
		t.Fatal("expected non-empty access_token")
	}
	if resp["token_type"] != "Bearer" {
		t.Errorf("token_type = %v", resp["token_type"])
	}
	if resp["expires_in"] != float64(86400) {
		t.Errorf("expires_in = %v, want 86400", resp["expires_in"])
	}
	if !tokens.Validate(raw) {
		t.Error("issued token should be valid in apitoken store")
	}
}

func TestToken_Replay_Returns400(t *testing.T) {
	h, _ := makeHandlers(t)
	code, verifier := fullPKCECode(t, h)

	exchange := func() int {
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"code_verifier": {verifier},
			"redirect_uri":  {"http://localhost:12345/callback"},
			"client_id":     {"cursor"},
		}
		req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		h.Token().ServeHTTP(w, req)
		return w.Code
	}
	if exchange() != http.StatusOK {
		t.Fatal("first exchange should succeed")
	}
	if exchange() != http.StatusBadRequest {
		t.Fatal("replay should return 400")
	}
}

func TestToken_WrongVerifier_Returns400(t *testing.T) {
	h, _ := makeHandlers(t)
	code, _ := fullPKCECode(t, h)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {"wrong-verifier"},
		"redirect_uri":  {"http://localhost:12345/callback"},
		"client_id":     {"cursor"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Token().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	body, _ := io.ReadAll(w.Body)
	if !strings.Contains(string(body), "invalid_grant") {
		t.Errorf("want invalid_grant in body, got: %s", body)
	}
}

func TestToken_ClientIDMismatch_Returns400(t *testing.T) {
	h, _ := makeHandlers(t)
	code, verifier := fullPKCECode(t, h)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://localhost:12345/callback"},
		"client_id":     {"wrong-client"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Token().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestToken_RedirectURIMismatch_Returns400(t *testing.T) {
	h, _ := makeHandlers(t)
	code, verifier := fullPKCECode(t, h)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://localhost:9999/other"},
		"client_id":     {"cursor"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Token().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// --- A1: the consent POST needs an authenticated browser (tether#117) ---

// TestAuthorizePost_NoSession_Returns401 is the head of the attack chain the
// wi documents. Three requests used to mint a 24h MCP Bearer token with zero
// credentials:
//
//  1. GET  /oauth/authorize?...&redirect_uri=http://127.0.0.1:1/ → req_id
//  2. POST /oauth/authorize  req_id=…&action=allow               → 302 with ?code=
//  3. POST /oauth/token      code + code_verifier                → access_token
//
// Step 2 is the one that needs a human at a keyboard, and it is the one that
// asked for nothing. Note step 1's redirect_uri never has to be reachable: the
// code arrives in the Location header of step 2's response.
func TestAuthorizePost_NoSession_Returns401(t *testing.T) {
	h, _ := makeHandlers(t)
	reqID := reqIDFromApprovalPage(t, h)

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "203.0.113.7:44444" // a remote peer, as on a --acme-domain deployment
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", w.Code, w.Body.String())
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("no redirect must be issued; got Location: %s", loc)
	}
	if strings.Contains(w.Body.String(), "code=") {
		t.Error("response body must not carry an authorization code")
	}
}

// TestAuthorizePost_NoSession_DoesNotBurnReqID is why the session check sits
// BEFORE ConsumePending. req_ids are single-use, so an unauthenticated POST that
// consumed one would let anyone who can guess or observe a req_id cancel the
// owner's approval mid-flow.
func TestAuthorizePost_NoSession_DoesNotBurnReqID(t *testing.T) {
	h, _ := makeHandlers(t)
	reqID := reqIDFromApprovalPage(t, h)

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	unauth := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
	unauth.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w1 := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w1, unauth)
	if w1.Code != http.StatusUnauthorized {
		t.Fatalf("setup: want 401, got %d", w1.Code)
	}

	// The same req_id must still work for the signed-in owner.
	authed := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	authed.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w2, authed)
	if w2.Code != http.StatusFound {
		t.Fatalf("the rejected POST consumed the req_id: want 302, got %d: %s", w2.Code, w2.Body.String())
	}
}

// TestAuthorizePost_NilSessionAuth_Returns401 pins the fail-closed default: a
// caller that forgets to wire SessionAuth gets a broken approval flow, never a
// public mint.
func TestAuthorizePost_NilSessionAuth_Returns401(t *testing.T) {
	h := makeHandlersNoSessionAuth(t)
	reqID := reqIDFromApprovalPage(t, h)

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("nil SessionAuth must deny: want 401, got %d", w.Code)
	}
}

// TestAuthorizeGet_NoSession_StillShowsApprovalPage keeps the GET exemption
// deliberate rather than incidental. The MCP client opens this URL in the
// owner's browser and that browser may not hold the cookie yet; requiring one
// here would replace the consent page with a login redirect and lose the
// pending request. Nothing is minted by rendering the page.
func TestAuthorizeGet_NoSession_StillShowsApprovalPage(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	req := httptest.NewRequest("GET", authorizeURL(challenge, nil), nil)
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("GET without a session: want 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `name="req_id"`) {
		t.Error("consent page should still render for an unauthenticated GET")
	}
}

// TestToken_NoSession_StillExchanges keeps the /oauth/token exemption
// deliberate too: the code is single-use and bound to a PKCE challenge, so the
// exchange authenticates itself, and the MCP client that performs it has no
// cookie jar. Requiring a session here would break every remote client without
// adding a guard the code+verifier pair does not already give.
func TestToken_NoSession_StillExchanges(t *testing.T) {
	h, _ := makeHandlers(t)
	code, verifier := fullPKCECode(t, h)

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://localhost:12345/callback"},
		"client_id":     {"cursor"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Token().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("cookie-less token exchange must still work: got %d: %s", w.Code, w.Body.String())
	}
}
