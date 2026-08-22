package oauth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
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

// reqIDMarker is the consent page's req_id input, up to the value. Only the
// consent page contains it, which is what makes it usable both as the anchor
// these helpers scan for and as the assertion that a consent page is what came
// back at all.
const reqIDMarker = `name="req_id" value="`

// reqIDFromRecorder pulls the consent page's req_id out of an authorize GET
// response, or fails the test with the response it actually got.
//
// Every helper here used to do this inline as three unchecked strings.Index
// calls, which indexed into the body with the result of a FAILED lookup:
// start became -1+len(marker) and end became -1, so any response that was not a
// consent page produced a slice-bounds panic. A panic in a test helper does not
// fail one test — it takes down the whole test binary, so every result that had
// not been printed yet disappears and the run's failure list is silently a
// truncated one. That is exactly how it was found (tether#117's mutation run,
// filed as tether#155): the panic read like a defect in the code under test,
// and the short red list nearly supported the wrong conclusion.
//
// So the contract is: a helper that cannot get what it came for reports what it
// saw and fails ONLY its own test.
func reqIDFromRecorder(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	body := w.Body.String()
	start := strings.Index(body, reqIDMarker)
	if start < 0 {
		t.Fatalf("authorize GET returned no consent page (no %q in body): status %d, body %s",
			reqIDMarker, w.Code, bodyExcerpt(body))
	}
	start += len(reqIDMarker)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatalf("consent page's req_id value is unterminated (no closing quote): status %d, body %s",
			w.Code, bodyExcerpt(body))
	}
	return body[start : start+end]
}

// bodyExcerpt renders a response body for a failure message: quoted, so control
// characters and an empty body are visible rather than blending into the line,
// and truncated, so a large page cannot bury the message it is attached to.
func bodyExcerpt(body string) string {
	const max = 400
	if len(body) <= max {
		return strconv.Quote(body)
	}
	return strconv.Quote(body[:max]) + fmt.Sprintf(" … (truncated, %d bytes total)", len(body))
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
	// withSession since tether#153: a signed-out GET gets the sign-in prompt, and
	// what this test is about is the consent page.
	req := withSession(httptest.NewRequest("GET", authorizeURL(challenge, nil), nil))
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

// TestAuthorizeGet_XSSClientID_IsEscaped pins that client_id — an
// attacker-chosen string that arrives in the query string and is rendered into
// the consent page — cannot break out of the surrounding HTML.
//
// The order of the assertions is the point. This test used to consist of one
// negative: the body does not contain "<script>". That is equally true when the
// escaping works and when no page is rendered at all — a 401, a 400, an empty
// body all pass it — so it held in both the intact and the broken state, which
// is another way of saying it was not a gate (tether#155). Verified: with an
// authorizeGet that answers 401 and renders nothing, the old assertion stayed
// green.
//
// Hence: establish that a consent page came back FIRST, and only then look at
// how it rendered the injected string. The escaping assertion is written as
// "the escaped form is present", not merely "the raw form is absent", because
// the presence form has an answer to "what would this value be if the defect
// were there?" — with the escaping gone the body carries the raw string and
// this substring is missing.
//
// The GET carries a session, which the old version did not need because it
// asserted nothing about what came back. Now that it demands a rendered consent
// page, the entry it uses matters: whether an UNAUTHENTICATED GET renders the
// page is a live question that tether#153 is changing, and it is not this
// test's question. What this test guards is the escaping, and "a signed-in GET
// renders the consent page" is the premise that holds either way.
func TestAuthorizeGet_XSSClientID_IsEscaped(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	xssID := `<script>alert(1)</script>`
	req := withSession(httptest.NewRequest("GET", authorizeURL(challenge, url.Values{"client_id": {xssID}}), nil))
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	// 1. A consent page was rendered. Without this, everything below is vacuous.
	if w.Code != http.StatusOK {
		t.Fatalf("want a rendered consent page (200), got %d: %s", w.Code, bodyExcerpt(w.Body.String()))
	}
	body := w.Body.String()
	if !strings.Contains(body, reqIDMarker) {
		t.Fatalf("200 but not the consent page (no %q): %s", reqIDMarker, bodyExcerpt(body))
	}

	// 2. That consent page rendered the attacker's client_id, escaped.
	const escaped = "&lt;script&gt;alert(1)&lt;/script&gt;"
	if !strings.Contains(body, escaped) {
		t.Errorf("client_id must appear HTML-escaped (%q) in the consent page; got %s", escaped, bodyExcerpt(body))
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("a raw <script> tag must never reach the consent page; got %s", bodyExcerpt(body))
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
	// withSession since tether#153: only a signed-in GET renders the form that
	// carries a req_id.
	req := withSession(httptest.NewRequest("GET", authorizeURL(challenge, nil), nil))
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)
	return reqIDFromRecorder(t, w)
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

// fullPKCECode performs GET+POST authorize and returns the issued code +
// verifier. Every step it depends on is checked: a helper that hands back an
// empty code because the flow it drives stopped working turns its callers'
// failures into a puzzle about the token endpoint.
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
	// withSession since tether#153 — see reqIDFromApprovalPage.
	req := withSession(httptest.NewRequest("GET", "/oauth/authorize?"+v.Encode(), nil))
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)
	reqID := reqIDFromRecorder(t, w)

	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req2 := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w2 := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w2, req2)
	if w2.Code != http.StatusFound {
		t.Fatalf("consent POST did not redirect: want 302, got %d: %s", w2.Code, bodyExcerpt(w2.Body.String()))
	}
	loc := w2.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("consent POST redirected to an unparseable Location %q: %v", loc, err)
	}
	code = u.Query().Get("code")
	if code == "" {
		t.Fatalf("consent POST redirect carried no code: Location %q", loc)
	}
	return code, verifier
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
//
// Since tether#153 a nil SessionAuth also denies the GET, so there is no consent
// page to lift a req_id from and this posts an arbitrary one. That does not
// weaken the assertion: 401 and 400 are different answers, and an unknown req_id
// is what produces the 400 (from ConsumePending) if the session check ever stops
// running first.
func TestAuthorizePost_NilSessionAuth_Returns401(t *testing.T) {
	h := makeHandlersNoSessionAuth(t)

	form := url.Values{"req_id": {"any-req-id"}, "action": {"allow"}}
	req := withSession(httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode())))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("nil SessionAuth must deny: want 401, got %d", w.Code)
	}
}

// The GET exemption in internal/auth/middleware.go is unchanged and still
// deliberate: this endpoint is reachable without a cookie, and nothing is minted
// by reaching it. What a cookie-less GET now RENDERS moved in tether#153 —
// TestAuthorizeGet_NoSession_StillShowsApprovalPage used to pin the consent page
// here and has been superseded by signin_prompt_test.go, which pins the sign-in
// prompt and its return link. The reason is in that file: showing consent to a
// signed-out browser only moved the failure one click later, since the POST is
// session-gated and the /auth redirect behind it carries no ?redirect=.

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
