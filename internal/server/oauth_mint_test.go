package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/auth/oauth"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

// tether#117 A1, end to end — the composition, not the parts.
//
// internal/auth pins the middleware's method-aware exemption and
// internal/auth/oauth pins the handler's own session check, each against a stub
// of the other side. Neither covers the hop between them, and that hop is
// hand-written in Run(): the middleware has to actually wrap the OAuth routes,
// and SessionAuth has to actually be the cookie verifier. Both could be perfect
// in isolation while the daemon still mints tokens for strangers.
//
// So this replays the real attack from the wi against the real buildMux.

// mintChainMux builds a daemon the way Run() does for the parts that matter
// here: one auth.State, and oauth.Handlers whose SessionAuth is that same
// auth.State's cookie verifier — the identical expression lifecycle.go passes.
func mintChainMux(t *testing.T) (http.Handler, *apitoken.Store, string) {
	t.Helper()
	reg := session.NewRegistry()
	reg.History = session.NewHistoryStore(t.TempDir())
	cfg := &Config{Port: 8899, Registry: reg, MCPLifecycle: mcplifecycle.New()}

	secret := []byte("test-secret-for-the-oauth-mint-chain")
	authState := auth.NewState("test-token", secret)
	sessionCookie, err := auth.IssueJWT(secret, "owner-browser")
	if err != nil {
		t.Fatalf("mint a session cookie: %v", err)
	}

	tokens, err := apitoken.Open(filepath.Join(t.TempDir(), "api-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	// sessionAuthFor, not a retyped closure: the wiring is what this file exists
	// to test, so it has to be the production one.
	oauthH := oauth.NewHandlers(oauth.NewCodeStore(), tokens, "https://127.0.0.1:8899",
		sessionAuthFor(authState))

	mux := buildMux(cfg, newCertHolder(mustGenCert(t)), nil, reg, nil, authState,
		nil, nil, oauthH, cfg.MCPLifecycle)
	return mux, tokens, sessionCookie
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// signInAnchor appears only on the sign-in prompt tether#153 renders for a
// cookie-less GET. Reaching it through the real mux is what proves the two
// halves are still wired the way they are meant to be: the middleware's GET
// exemption has to let the request through (a middleware that gated GET too
// would answer 302 /auth and the handler would never run), and the handler has to
// be the one that decides what a signed-out browser sees.
const signInAnchor = `id="oauth-signin-continue"`

// authorizeGetStep builds step 1 of the chain. cookie is the tether_session
// value, or "" for the cookie-less shape.
func authorizeGetStep(challenge, cookie string) *http.Request {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {"attacker-cli"},
		"redirect_uri":          {"http://127.0.0.1:1/"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	req := httptest.NewRequest("GET", "/oauth/authorize?"+v.Encode(), nil)
	req.RemoteAddr = "203.0.113.7:44444" // a remote peer, as on a --acme-domain deployment
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	}
	return req
}

// authorizeGetReqID performs step 1 of the chain and returns the req_id from the
// consent page. The cookie is required since tether#153: the endpoint is still
// exempt from the middleware, but a signed-out browser is shown the sign-in
// prompt rather than a consent form, so there is no req_id to be had without one.
func authorizeGetReqID(t *testing.T, mux http.Handler, challenge, cookie string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, authorizeGetStep(challenge, cookie))
	if rr.Code != http.StatusOK {
		t.Fatalf("step 1 (GET consent page): want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	const marker = `name="req_id" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("step 1: no req_id in the consent page:\n%s", body)
	}
	rest := body[i+len(marker):]
	return rest[:strings.Index(rest, `"`)]
}

// consentPost builds step 2. origin selects the shape of the caller: "" is the
// curl shape (no Origin header at all, which is how the attack arrives and the
// reason WithOriginGuard never stopped it — that guard only rejects an Origin
// that is present AND disallowed), and a non-empty origin is the browser shape,
// which the real consent form sends and which therefore has to clear
// originAllowed as well as the cookie check.
func consentPost(reqID, cookie, origin string) *http.Request {
	form := url.Values{"req_id": {reqID}, "action": {"allow"}}
	req := httptest.NewRequest("POST", "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// A remote peer, as on a --acme-domain deployment.
	req.RemoteAddr = "203.0.113.7:44444"
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: cookie})
	}
	return req
}

// TestOAuthMintChain_NoCredentials_YieldsNoToken replays all three requests with
// zero credentials and asserts no token exists at the end.
//
// Since tether#153 the chain dies twice over: step 1 no longer yields a req_id
// without a cookie, and step 2 still refuses one that was obtained with a cookie
// and spent without. Both are asserted, because the first is a UX change and only
// the second is load-bearing — a later revision that reopened step 1 must not be
// able to quietly take the mint gate with it.
func TestOAuthMintChain_NoCredentials_YieldsNoToken(t *testing.T) {
	mux, tokens, cookie := mintChainMux(t)
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

	// Step 1, with no credentials, now ends the chain here (tether#153): there is
	// no consent form, so there is no req_id to carry into step 2.
	//
	// The status is what separates the two ways this could look identical from
	// the outside. 401 with the prompt in the body means the middleware still
	// exempts GET and the handler answered; 302 would mean the exemption is gone
	// and the middleware answered, which is the change tether#117 A1 explicitly
	// warned against making to "fix" this dead end.
	rr1 := httptest.NewRecorder()
	mux.ServeHTTP(rr1, authorizeGetStep(s256(verifier), ""))
	if rr1.Code != http.StatusUnauthorized {
		t.Fatalf("step 1 without a cookie: want 401 from the handler, got %d (302 would mean the middleware's GET exemption was dropped): %s",
			rr1.Code, rr1.Header().Get("Location"))
	}
	if !strings.Contains(rr1.Body.String(), signInAnchor) {
		t.Errorf("step 1 should render the sign-in prompt:\n%s", rr1.Body.String())
	}
	if strings.Contains(rr1.Body.String(), `name="req_id"`) {
		t.Error("step 1 handed a req_id to a caller with no credentials")
	}

	// The POST gate is the load-bearing one and has to hold on its own, so the
	// rest of the chain is replayed against a req_id that really exists. A req_id
	// is not a credential — it is a lookup key the owner's own browser was shown —
	// so obtaining one with a cookie and spending it without one is exactly the
	// attack step 2 must refuse.
	reqID := authorizeGetReqID(t, mux, s256(verifier), cookie)

	// Step 2 — the mint. This is the request that used to answer 302 with the
	// code sitting in the Location header, which is why the redirect_uri above
	// never had to be reachable.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, consentPost(reqID, "", ""))

	loc := rr.Header().Get("Location")
	if strings.Contains(loc, "code=") {
		t.Fatalf("step 2 handed out an authorization code with no credentials: %s", loc)
	}
	if rr.Code == http.StatusFound && loc != "/auth" {
		t.Fatalf("step 2 redirected somewhere other than the login page: %d -> %s", rr.Code, loc)
	}
	if rr.Code != http.StatusFound && rr.Code != http.StatusUnauthorized {
		t.Fatalf("step 2: want 302 to /auth or 401, got %d: %s", rr.Code, rr.Body.String())
	}

	// Step 3 — with no code there is nothing to exchange. Asserted rather than
	// assumed: the value of this test is that NO token exists at the end.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {""},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://127.0.0.1:1/"},
		"client_id":     {"attacker-cli"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req)
	if rr3.Code == http.StatusOK {
		t.Fatalf("step 3 issued a token: %s", rr3.Body.String())
	}

	// And nothing was persisted along the way.
	if held := tokens.List(); len(held) != 0 {
		t.Fatalf("the api-token store holds %d token(s) after an unauthenticated chain", len(held))
	}
}

// TestOAuthMintChain_WithSession_StillIssuesAToken is the other half: the fix
// must not break the flow it is protecting. Same three requests, one cookie.
func TestOAuthMintChain_WithSession_StillIssuesAToken(t *testing.T) {
	mux, tokens, cookie := mintChainMux(t)
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

	reqID := authorizeGetReqID(t, mux, s256(verifier), cookie)

	rr := httptest.NewRecorder()
	// The browser shape: the real consent form is served from this daemon, so it
	// sends an Origin, and the request must clear WithOriginGuard too.
	mux.ServeHTTP(rr, consentPost(reqID, cookie, "https://127.0.0.1:8899"))
	if rr.Code != http.StatusFound {
		t.Fatalf("consent POST from a signed-in browser: want 302, got %d: %s", rr.Code, rr.Body.String())
	}
	loc, err := url.Parse(rr.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in the redirect: %s", rr.Header().Get("Location"))
	}

	// The exchange carries NO cookie — /oauth/token stays exempt on purpose, and
	// this is the assertion that keeps it that way.
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {"http://127.0.0.1:1/"},
		"client_id":     {"attacker-cli"},
	}
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr3 := httptest.NewRecorder()
	mux.ServeHTTP(rr3, req)
	if rr3.Code != http.StatusOK {
		t.Fatalf("token exchange: want 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(rr3.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	raw, _ := resp["access_token"].(string)
	if raw == "" {
		t.Fatal("no access_token in the response")
	}
	if !tokens.Validate(raw) {
		t.Error("the issued token does not validate against the store")
	}
}

// TestOAuthMintChain_WTTicketCookie_YieldsNoToken joins A1 to A2. The consent
// POST is now cookie-gated, so the next question is what counts as a cookie —
// and a 60-second WebTransport ticket, which travels in a URL query param and
// therefore leaks into logs and Referer headers, must not.
func TestOAuthMintChain_WTTicketCookie_YieldsNoToken(t *testing.T) {
	reg := session.NewRegistry()
	reg.History = session.NewHistoryStore(t.TempDir())
	cfg := &Config{Port: 8899, Registry: reg, MCPLifecycle: mcplifecycle.New()}

	secret := []byte("test-secret-for-the-oauth-mint-chain")
	authState := auth.NewState("test-token", secret)
	ticket, err := auth.IssueWTTicket(secret, "owner-browser")
	if err != nil {
		t.Fatal(err)
	}
	// A real session cookie, used for step 1 only. Since tether#153 the consent
	// page needs one, and a wt-ticket is not one — which is the very thing this
	// test is about, so the ticket cannot double as it. Handing step 1 a genuine
	// cookie also keeps the question sharp: the ticket is refused at the mint on
	// its own merits, not because the chain never got that far.
	sessionCookie, err := auth.IssueJWT(secret, "owner-browser")
	if err != nil {
		t.Fatal(err)
	}

	tokens, err := apitoken.Open(filepath.Join(t.TempDir(), "api-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	oauthH := oauth.NewHandlers(oauth.NewCodeStore(), tokens, "https://127.0.0.1:8899",
		sessionAuthFor(authState))
	mux := buildMux(cfg, newCertHolder(mustGenCert(t)), nil, reg, nil, authState,
		nil, nil, oauthH, cfg.MCPLifecycle)

	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	reqID := authorizeGetReqID(t, mux, s256(verifier), sessionCookie)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, consentPost(reqID, ticket, "https://127.0.0.1:8899"))
	if strings.Contains(rr.Header().Get("Location"), "code=") {
		t.Fatalf("a wt-ticket in the session cookie minted a code: %s", rr.Header().Get("Location"))
	}
	if held := tokens.List(); len(held) != 0 {
		t.Fatalf("the api-token store holds %d token(s) after a wt-ticket chain", len(held))
	}
}
