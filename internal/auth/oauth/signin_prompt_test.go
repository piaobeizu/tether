package oauth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tether#153 — what an unauthenticated GET /oauth/authorize renders.
//
// tether#117 A1 put a session-cookie gate on the consent POST and left the GET
// exempt, so a signed-out browser still got the consent page, clicked Allow, and
// hit a 401 it could not recover from: the middleware sends a non-API request to
// /auth with no ?redirect=, so the owner lands on / and the pending request is
// gone. The consent page only made the failure arrive one click later.
//
// This file pins the replacement: the sign-in step now comes FIRST, and the whole
// original request rides along in /auth?redirect= so the consent page comes back
// intact.
//
// These assertions are deliberately two-sided. "The body does not contain the
// consent form" is true of a 401 with an empty body, a 500, and a panic, so on its
// own it is not a gate at all — the same defect class tether#155 is fixing one
// file over. Every check below therefore names what the response IS, and the
// comment on each says what it evaluated to while the bug was live.

// signInAnchor appears only on the sign-in prompt; consentAnchor appears only on
// the consent page. Keeping both, and asserting on both in both directions, is
// what makes these tests able to tell the two pages apart rather than merely
// notice that one of them is missing.
const (
	signInAnchor  = `id="oauth-signin-continue"`
	consentAnchor = `name="req_id"`
	consentForm   = `<form method="POST" action="/oauth/authorize">`
)

// getWithRawQuery builds GET /oauth/authorize whose RawQuery is exactly raw.
// It assigns RawQuery after parsing rather than embedding it in the URL string,
// so bytes that net/url would otherwise consume (a literal '#' starts a fragment)
// still reach the handler verbatim.
func getWithRawQuery(raw string) *http.Request {
	req := httptest.NewRequest("GET", "/oauth/authorize", nil)
	req.URL.RawQuery = raw
	return req
}

// signInHref returns the href of the sign-in link in body.
//
// It fails the test instead of slicing blindly: a helper that indexes into a body
// it did not find the marker in takes the whole test binary down with it and
// truncates the failure list, which is how one bad assertion hides the other
// nineteen (tether#155).
func signInHref(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, signInAnchor)
	if i < 0 {
		t.Fatalf("no sign-in anchor %s in the response body:\n%s", signInAnchor, body)
	}
	rest := body[i:]
	const marker = `href="`
	j := strings.Index(rest, marker)
	if j < 0 {
		t.Fatalf("sign-in anchor carries no href:\n%s", body)
	}
	rest = rest[j+len(marker):]
	k := strings.Index(rest, `"`)
	if k < 0 {
		t.Fatalf("unterminated href on the sign-in anchor:\n%s", body)
	}
	return rest[:k]
}

// wantSignInURL is the expected /auth?redirect= for a request whose RawQuery is
// raw. It is spelled out here rather than imported from the handler so the test
// pins a value instead of restating an implementation; the corpus in
// testdata/signin_redirect_corpus.json pins the same strings a third time, as
// literals, and is shared with the JS consumer.
func wantSignInURL(raw string) string {
	return "/auth?redirect=" + url.QueryEscape("/oauth/authorize?"+raw)
}

// TestAuthorizeGet_NoSession_RendersSignInPromptNotConsentPage is the gate: it
// has to separate "signed out, told to sign in" from "signed out, shown the
// consent page", and from a bare error page that is neither.
func TestAuthorizeGet_NoSession_RendersSignInPromptNotConsentPage(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	raw := strings.TrimPrefix(authorizeURL(challenge, nil), "/oauth/authorize?")

	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, getWithRawQuery(raw))
	body := w.Body.String()

	// Was 200 while the bug was live — the consent page rendered happily.
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET: want 401, got %d\n%s", w.Code, body)
	}
	// Was "text/html; charset=utf-8" then too. Asserted because the two checks
	// below only mean anything about a rendered page if a page was rendered.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// Was absent (the anchor did not exist). This is the positive half: a 401
	// with no body, or a 500, fails here, so "not the consent page" cannot pass
	// by being nothing at all.
	if !strings.Contains(body, signInAnchor) {
		t.Errorf("sign-in prompt not rendered: no %s in\n%s", signInAnchor, body)
	}
	// Was present, twice over: the form and its hidden req_id are exactly what
	// let the owner click Allow into a dead end.
	if strings.Contains(body, consentAnchor) {
		t.Errorf("consent page rendered to a signed-out browser: found %s", consentAnchor)
	}
	if strings.Contains(body, consentForm) {
		t.Errorf("consent form rendered to a signed-out browser")
	}
	// Was absent. The return trip is the entire point of the change, so the
	// exact string has to be in the page, not merely some link to /auth.
	if got, want := signInHref(t, body), wantSignInURL(raw); got != want {
		t.Errorf("sign-in href\n got: %s\nwant: %s", got, want)
	}
}

// TestAuthorizeGet_WithSession_StillRendersConsentPage is the other side of the
// same gate. Without it, deleting the consent page outright would pass every
// assertion above.
func TestAuthorizeGet_WithSession_StillRendersConsentPage(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	req := withSession(httptest.NewRequest("GET", authorizeURL(challenge, nil), nil))

	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)
	body := w.Body.String()

	if w.Code != http.StatusOK {
		t.Fatalf("signed-in GET: want 200, got %d\n%s", w.Code, body)
	}
	if !strings.Contains(body, consentAnchor) {
		t.Errorf("consent page missing %s:\n%s", consentAnchor, body)
	}
	if !strings.Contains(body, consentForm) {
		t.Errorf("consent form missing:\n%s", body)
	}
	if strings.Contains(body, signInAnchor) {
		t.Errorf("a signed-in browser was told to sign in")
	}
}

// TestAuthorizeGet_NilSessionAuth_RendersSignInPrompt keeps the GET fail-closed
// in the same direction the POST already is (TestAuthorizePost_NilSessionAuth_
// Returns401): an embedder that forgets to wire SessionAuth gets a flow that
// asks for a login, never one that quietly hands out consent pages.
func TestAuthorizeGet_NilSessionAuth_RendersSignInPrompt(t *testing.T) {
	h := makeHandlersNoSessionAuth(t)
	_, challenge := pkceTestPair()
	// A session cookie is present and still must not help: SessionAuth is nil,
	// so there is nothing to verify it with.
	req := withSession(httptest.NewRequest("GET", authorizeURL(challenge, nil), nil))

	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("nil SessionAuth on GET: want 401, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), signInAnchor) {
		t.Errorf("nil SessionAuth should render the sign-in prompt")
	}
}

// TestAuthorizeGet_NoSession_EscapesClientID covers the new page's own injection
// surface. client_id is attacker-chosen and this template prints it, so the
// escaping question does not go away just because the consent page already
// answered it — this is a different template.
//
// The assertion names what IS in the body (the escaped form) as well as what is
// not, because "no <script> in the body" is also true of the 401 error page that
// preceded this change.
func TestAuthorizeGet_NoSession_EscapesClientID(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	const xssID = `<script>alert(1)</script>`
	raw := strings.TrimPrefix(authorizeURL(challenge, url.Values{"client_id": {xssID}}), "/oauth/authorize?")

	w := httptest.NewRecorder()
	h.Authorize().ServeHTTP(w, getWithRawQuery(raw))
	body := w.Body.String()

	if !strings.Contains(body, signInAnchor) {
		t.Fatalf("expected the sign-in prompt, got:\n%s", body)
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("client_id must be HTML-escaped in the sign-in prompt")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("client_id should still be shown, escaped; body:\n%s", body)
	}
}

// TestAuthorizeGet_InvalidRequest_ReportsTheOAuthErrorWithoutASession pins where
// the session check sits: after request validation, before StorePending.
//
// After validation, because a malformed authorization request is malformed
// whether or not anyone is signed in, and telling the caller to log in first
// would only replay the same error after the round trip. Before StorePending,
// because a pending record created here would be an orphan — the browser comes
// back through this same handler and stores a fresh one.
func TestAuthorizeGet_InvalidRequest_ReportsTheOAuthErrorWithoutASession(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()

	t.Run("non-loopback redirect_uri still 400s", func(t *testing.T) {
		raw := strings.TrimPrefix(
			authorizeURL(challenge, url.Values{"redirect_uri": {"https://evil.example.com/steal"}}),
			"/oauth/authorize?")
		w := httptest.NewRecorder()
		h.Authorize().ServeHTTP(w, getWithRawQuery(raw))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), signInAnchor) {
			t.Error("a bad redirect_uri should be reported, not swallowed by a login prompt")
		}
	})

	t.Run("plain PKCE still redirects with invalid_request", func(t *testing.T) {
		raw := strings.TrimPrefix(
			authorizeURL(challenge, url.Values{"code_challenge_method": {"plain"}}),
			"/oauth/authorize?")
		w := httptest.NewRecorder()
		h.Authorize().ServeHTTP(w, getWithRawQuery(raw))
		if w.Code != http.StatusFound {
			t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Location"), "error=invalid_request") {
			t.Errorf("want invalid_request, got %s", w.Header().Get("Location"))
		}
	})
}

// TestAuthorizeGet_SignInRedirect_IgnoresTheRequestPath is the reason the return
// target is assembled from a constant.
//
// r.URL.Path is client-controlled. `/..//evil.example` parses to OUR origin with
// a protocol-relative pathname, which is precisely the shape that broke the first
// version of safeRedirectTarget in tether#117 A3 — so a return target echoing the
// request path would have handed the browser an off-site navigation dressed up as
// a same-origin one. Building it from the constant makes that unreachable rather
// than merely unlikely, and this asserts the constant is what is used.
func TestAuthorizeGet_SignInRedirect_IgnoresTheRequestPath(t *testing.T) {
	h, _ := makeHandlers(t)
	_, challenge := pkceTestPair()
	raw := strings.TrimPrefix(authorizeURL(challenge, nil), "/oauth/authorize?")

	for _, path := range []string{"/..//evil.example", "//evil.example", "/oauth/authorize/../..//evil.example"} {
		req := getWithRawQuery(raw)
		req.URL.Path = path
		w := httptest.NewRecorder()
		h.Authorize().ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("path %q: want 401, got %d", path, w.Code)
		}
		if got, want := signInHref(t, w.Body.String()), wantSignInURL(raw); got != want {
			t.Errorf("path %q leaked into the return target\n got: %s\nwant: %s", path, got, want)
		}
	}
}

// --- the cross-language corpus ---

type signInCorpusEntry struct {
	Label        string            `json:"label"`
	Why          string            `json:"why"`
	RawQuery     string            `json:"rawQuery"`
	WantAuthURL  string            `json:"wantAuthURL"`
	ExpectParams map[string]string `json:"expectParams"`
}

const signInCorpusPath = "testdata/signin_redirect_corpus.json"

func loadSignInCorpus(t *testing.T) []signInCorpusEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(signInCorpusPath))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var doc struct {
		Entries []signInCorpusEntry `json:"entries"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	return doc.Entries
}

// TestSignInRedirectCorpus_MatchesHandlerOutput is the producer half of the
// contract in testdata/signin_redirect_corpus.json.
//
// The consumer of these strings is safeRedirectTarget() in web/src/AuthPage.tsx,
// which is JS and cannot call this handler. The alternative — a list of literals
// hand-copied into each language — is how two sides end up holding different
// subsets while both look complete, so the file is the single copy: this test
// asserts the handler still produces it, and web/src/oauthSignInRedirect.test.ts
// asserts the real consumer still accepts it. Change the producer and this goes
// red until the file is updated, at which point the JS side re-validates the new
// strings.
func TestSignInRedirectCorpus_MatchesHandlerOutput(t *testing.T) {
	h, _ := makeHandlers(t)
	entries := loadSignInCorpus(t)

	for _, e := range entries {
		t.Run(e.Label, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.Authorize().ServeHTTP(w, getWithRawQuery(e.RawQuery))

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("want 401 (%s), got %d: %s", e.Why, w.Code, w.Body.String())
			}
			// The rendered href, not the return value of a helper: html/template
			// applies URL-context escaping to this attribute, and an escaping that
			// mangled the target would be invisible to a unit test of the builder.
			if got := signInHref(t, w.Body.String()); got != e.WantAuthURL {
				t.Errorf("corpus drift (%s)\n got: %s\nwant: %s", e.Why, got, e.WantAuthURL)
			}
		})
	}
}

// TestSignInRedirectCorpus_IsPopulated guards the guard. A corpus that silently
// shrinks to zero entries makes every loop above a no-op that passes, and the
// two shapes named here are the ones tether#117 A3 was actually wrong about, so
// their absence has to be an error rather than a smaller run.
func TestSignInRedirectCorpus_IsPopulated(t *testing.T) {
	entries := loadSignInCorpus(t)
	if len(entries) < 8 {
		t.Fatalf("corpus has %d entries, want at least 8", len(entries))
	}
	required := []string{
		"dot-dot-segments-raw-in-a-value",
		"dot-dot-segments-percent-encoded-in-redirect-uri",
		"same-origin-absolute-url-raw",
		"same-origin-absolute-url-percent-encoded",
	}
	have := make(map[string]bool, len(entries))
	for _, e := range entries {
		have[e.Label] = true
		if e.RawQuery == "" || e.WantAuthURL == "" {
			t.Errorf("entry %q is missing rawQuery or wantAuthURL", e.Label)
		}
	}
	for _, label := range required {
		if !have[label] {
			t.Errorf("corpus lost its %q case", label)
		}
	}
}
