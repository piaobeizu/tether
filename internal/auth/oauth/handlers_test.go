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

func makeHandlers(t *testing.T) (*oauth.Handlers, *apitoken.Store) {
	t.Helper()
	tokens, err := apitoken.Open(filepath.Join(t.TempDir(), "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	cs := oauth.NewCodeStore()
	h := oauth.NewHandlers(cs, tokens, "https://localhost:8897")
	return h, tokens
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
	req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
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
	req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
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
	req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
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
	req2 := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
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
