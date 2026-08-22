package server

// tether#103 — the session-activity endpoint, and above all its ROUTING.
//
// The routing half is not ceremony. This daemon has two patterns whose text
// differs from this one by a hyphen or a plural, one of them a PREFIX handler,
// and the wrong answer to "which one wins" is silent:
//
//	/api/v1/sessions          exact    the list
//	/api/v1/sessions/         PREFIX   sessionSub — every path under it is a sid
//	/api/v1/                  PREFIX   the 501 stub
//	/api/v1/session-activity  exact    this
//
// A third neighbour used to sit in that table and was the sharpest of them:
// "/api/v1/session/" (singular!), a PREFIX handler serving handleLockForce.
// tether#121 unregistered it along with the shell input lock it served, so the
// old path now falls through to the 501 stub — which shell_lock_removed_test.go
// pins, in this same package.
//
// So the table below runs against buildMux itself. It has to: an earlier draft
// re-declared those patterns in this file, and deleting the real registration from
// mux.go then left the suite green with the whole feature unwired.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
	mcplifecycle "github.com/piaobeizu/tether/internal/mcp/lifecycle"
	"github.com/piaobeizu/tether/internal/session"
)

// activityRouteMux builds the daemon's REAL route table.
//
// buildMux, not a hand-rolled copy of its patterns. The first version of this test
// did re-declare them, and a review proved what that cost: deleting the
// `mux.HandleFunc(session.SessionActivityPath, …)` line from mux.go — unwiring the
// entire feature, so the endpoint falls through to the /api/v1/ 501 stub — left
// this file green. The frontend suite cannot catch it either, because every
// consumer test stubs fetch. So "the daemon actually serves this path" was guarded
// by nothing at all, which is the one thing a routing test is for.
//
// Nil is acceptable for most of buildMux's dependencies, and that is not a
// discovery of this test — cert_rotation_test.go and cert_external_test.go already
// call it this way for the same reason.
func activityRouteMux(t *testing.T, ccJobs *session.CCRegistry) (http.Handler, func(path string) *http.Request) {
	t.Helper()
	reg := session.NewRegistry()
	reg.CCJobs = ccJobs
	// A history store is what gates the /api/v1/sessions family (mux.go), and the
	// point of several rows below is that THOSE routes are unaffected — so the
	// daemon under test has to be one that serves them.
	reg.History = session.NewHistoryStore(t.TempDir())
	cfg := &Config{Port: 0, Registry: reg, MCPLifecycle: mcplifecycle.New()}

	// A REAL auth state, and a cookie for it. Not a detail: with a nil authState
	// buildMux's middleware rejects everything with 401, which is itself worth
	// knowing (this endpoint is inside the middleware, so it is not a hole in the
	// auth surface) but makes every routing assertion below say "unauthorized"
	// instead of naming the handler that answered.
	secret := []byte("test-secret-for-the-routing-table")
	authState := auth.NewState("test-token", secret)
	tok, err := auth.IssueJWT(secret, "routing-test")
	if err != nil {
		t.Fatalf("mint a session cookie: %v", err)
	}
	mux := buildMux(cfg, newCertHolder(mustGenCert(t)), nil, reg, nil, authState, nil, nil, nil, cfg.MCPLifecycle)

	return mux, func(path string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
		return r
	}
}

// TestSessionActivityRoute_IsBehindTheAuthMiddleware — the endpoint is polled every
// three seconds by every open browser, so "is it authenticated" is not a detail.
//
// Asserted against the real buildMux with NO credential, which is the same shape
// the live check produced: 401 and nothing else.
func TestSessionActivityRoute_IsBehindTheAuthMiddleware(t *testing.T) {
	mux, _ := activityRouteMux(t, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, session.SessionActivityPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET %s with no cookie -> %d, want 401; body: %s", session.SessionActivityPath, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// TestSessionActivityRoute_IsNotShadowedByItsNeighbours.
//
// The row that would actually have bitten is gone, and so is the hazard it
// guarded against: `/api/v1/session/` was a prefix handler, and "session-activity"
// is "session" plus a hyphen — but it was never inside that prefix, because the
// prefix ends in a slash. tether#121 unregistered that pattern with the shell
// lock, so there is no longer a shadowing question to assert here; what pins the
// pattern's absence is shell_lock_removed_test.go.
//
// The `/api/v1/sessions/activity` rows below record what the un-registered path
// did BEFORE this slice, which is worth pinning because it is not what it looks
// like: `/api/v1/sessions/activity` is refused with 400 by sessionSub's
// five-segment check, which runs BEFORE validSID — so "activity" is never treated
// as a sid there. It is treated as one at `/api/v1/sessions/activity/<leaf>`, five
// segments, and "activity" happens to satisfy validSID (8 alphanumerics). That is
// the real hazard in the neighbourhood and the reason this endpoint is not a leaf
// under /sessions/.
func TestSessionActivityRoute_IsNotShadowedByItsNeighbours(t *testing.T) {
	mux, req := activityRouteMux(t, nil)

	for _, tc := range []struct {
		path     string
		wantCode int
		wantBody string // substring
		why      string
	}{
		{
			session.SessionActivityPath, http.StatusOK, "{}",
			"the endpoint itself: REGISTERED by buildMux, and not swallowed by the /api/v1/ 501 stub",
		},
		{
			"/api/v1/session-activity/extra", http.StatusNotImplemented, "not implemented",
			"an EXACT pattern claims only that path; anything below it falls through to the stub, which is the honest answer for a route that takes no arguments",
		},
		{
			"/api/v1/sessions", http.StatusOK, "[]",
			"the list is untouched",
		},
		{
			"/api/v1/sessions/633e5ed8-cada-422a-aee1-c7a3502eb4fd/messages", http.StatusOK, "[]",
			"the transcript route is untouched — the reason this endpoint stayed out of that subtree",
		},
		{
			"/api/v1/sessions/activity", http.StatusBadRequest, "bad path",
			"NOT a 404 and NOT a sid: sessionSub's five-segment check refuses it before validSID is reached",
		},
		{
			"/api/v1/sessions/activity/messages", http.StatusOK, "[]",
			"five segments, and \"activity\" is 8 alphanumerics — so it IS accepted as a sid here, and served as an empty transcript. This is the hazard in the neighbourhood, stated as a fact rather than as a warning.",
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req(tc.path))
			if rec.Code != tc.wantCode {
				t.Fatalf("GET %s -> %d, want %d (%s); body: %s", tc.path, rec.Code, tc.wantCode, tc.why, strings.TrimSpace(rec.Body.String()))
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("GET %s body = %q, want it to contain %q (%s)", tc.path, strings.TrimSpace(rec.Body.String()), tc.wantBody, tc.why)
			}
		})
	}
}

// TestSessionActivity_ServesAJSONObjectKeyedBySid — the shape the SPA indexes.
//
// The empty answer must be `{}` and never `null`: the frontend reads
// `map[sid]`, and `null` is not indexable. That is a real hazard rather than a
// stylistic one, because a Go map that is nil marshals to `null` and a nil map is
// what any "return early" branch produces by default.
func TestSessionActivity_ServesAJSONObjectKeyedBySid(t *testing.T) {
	rec := httptest.NewRecorder()
	handleSessionActivity(&session.ActivityIndex{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, session.SessionActivityPath, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "{}" {
		t.Fatalf("empty answer = %q, want %q — a nil map marshals to `null`, which the SPA cannot index", got, "{}")
	}
	var into map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &into); err != nil {
		t.Fatalf("decode as map[string]string: %v", err)
	}
	// no-store, for the reason handleSessionActivity's doc gives: a cached answer
	// to "is it moving" is the frozen marker this whole slice exists to prevent.
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
}

// TestSessionActivity_RefusesNonSafeMethods — the same enforcement listSessions
// needed in tether#91, where it had been answering a DELETE with 200 and the whole
// list.
func TestSessionActivity_RefusesNonSafeMethods(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleSessionActivity(&session.ActivityIndex{}).ServeHTTP(rec, httptest.NewRequest(method, session.SessionActivityPath, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s -> %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleSessionActivity(&session.ActivityIndex{}).ServeHTTP(rec, httptest.NewRequest(method, session.SessionActivityPath, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s -> %d, want 200", method, rec.Code)
			}
		})
	}
}
