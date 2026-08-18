package server

// tether#103 — the session-activity endpoint, and above all its ROUTING.
//
// The routing half is not ceremony. This daemon has three patterns whose text
// differs by a hyphen or a plural, two of them PREFIX handlers, and the wrong
// answer to "which one wins" is silent:
//
//	/api/v1/sessions          exact    the list
//	/api/v1/sessions/         PREFIX   sessionSub — every path under it is a sid
//	/api/v1/session/          PREFIX   handleLockForce (singular!)
//	/api/v1/                  PREFIX   the 501 stub
//	/api/v1/session-activity  exact    this
//
// So the table below is written against the real registrations rather than
// against a reading of them.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/session"
)

// activityRouteMux registers the five patterns above with distinguishable
// handlers, using the SAME pattern strings and the SAME sub-handler parsing as
// buildMux and sessionAPIHandlers.
//
// Built here rather than by calling buildMux because buildMux needs a QUIC
// listener, an auth state, a cert holder and an MCP server to exist; what is under
// test is which pattern claims which path, and that is a property of the pattern
// set. The patterns are the load-bearing part, so they are string literals here
// and any divergence from mux.go shows up as this test asserting about a route the
// daemon does not have — which is why session.SessionActivityPath is used rather
// than re-typed.
func activityRouteMux(idx *session.ActivityIndex) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/session/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LOCK-FORCE"))
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})
	mux.HandleFunc("/api/v1/sessions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LIST"))
	})
	// sessionSub's real parsing, so the "activity is not a sid" claim is tested
	// against the code that would have to treat it as one.
	mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 5 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		if !validSID(parts[3]) {
			http.Error(w, "invalid sid", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("SUB sid=" + parts[3] + " leaf=" + parts[4]))
	})
	mux.HandleFunc(session.SessionActivityPath, handleSessionActivity(idx))
	return mux
}

// TestSessionActivityRoute_IsNotShadowedByItsNeighbours.
//
// The third row is the one that would actually have bitten: `/api/v1/session/` is
// a prefix handler, and "session-activity" is "session" plus a hyphen. It is NOT
// inside that prefix (the prefix ends in a slash), and this is the assertion that
// says so rather than the reasoning.
//
// The second row records what the un-registered path did BEFORE this slice, which
// is worth pinning because it is not what it looks like: `/api/v1/sessions/activity`
// is refused with 400 by sessionSub's five-segment check, which runs BEFORE
// validSID — so "activity" is never treated as a sid there. It is treated as one
// at `/api/v1/sessions/activity/<leaf>`, five segments, and "activity" happens to
// satisfy validSID (8 alphanumerics). That is the real hazard in the neighbourhood
// and the reason this endpoint is not a leaf under /sessions/.
func TestSessionActivityRoute_IsNotShadowedByItsNeighbours(t *testing.T) {
	mux := activityRouteMux(&session.ActivityIndex{})

	for _, tc := range []struct {
		path     string
		wantCode int
		wantBody string // substring
		why      string
	}{
		{
			session.SessionActivityPath, http.StatusOK, "{}",
			"the endpoint itself: reachable, and not swallowed by the /api/v1/ 501 stub",
		},
		{
			"/api/v1/session/whatever/force", http.StatusOK, "LOCK-FORCE",
			"the singular neighbour still owns its own subtree",
		},
		{
			"/api/v1/session-activity/extra", http.StatusNotImplemented, "not implemented",
			"an EXACT pattern claims only that path; anything below it falls through to the stub, which is the honest answer for a route that takes no arguments",
		},
		{
			"/api/v1/sessions", http.StatusOK, "LIST",
			"the list is untouched",
		},
		{
			"/api/v1/sessions/633e5ed8-cada-422a-aee1-c7a3502eb4fd/messages", http.StatusOK, "leaf=messages",
			"the transcript route is untouched — the reason this endpoint stayed out of that subtree",
		},
		{
			"/api/v1/sessions/activity", http.StatusBadRequest, "bad path",
			"NOT a 404 and NOT a sid: sessionSub's five-segment check refuses it before validSID is reached",
		},
		{
			"/api/v1/sessions/activity/messages", http.StatusOK, "sid=activity",
			"five segments, and \"activity\" is 8 alphanumerics — so it IS accepted as a sid here. This is the hazard in the neighbourhood, stated as a fact rather than as a warning.",
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
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
