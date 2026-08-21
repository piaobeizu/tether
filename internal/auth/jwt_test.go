package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/auth"
)

// testSecret is a fixed 32-byte HMAC key. Every token in this file is signed
// with it, which is the point: the defect these tests pin is NOT a signature
// forgery. Both token kinds are legitimately signed by the same daemon key —
// what distinguishes them is the `sub` claim, and nothing else.
var testSecret = []byte("0123456789abcdef0123456789abcdef")

// TestVerifyJWT_RejectsWTTicket pins tether#117 A2.
//
// IssueWTTicket and IssueJWT sign with the same secret and the same algorithm;
// the ONLY thing separating a 60-second WebTransport ticket from a 90-day
// session is `sub` ("wt-ticket" vs "tether"). VerifyWTTicket checks it in one
// direction — a session JWT cannot be spent as a ticket. VerifyJWT did not
// check the other, so a ticket (which travels in a ?ticket= URL query param, and
// therefore lands in proxy logs, browser history and Referer headers) could be
// pasted into the tether_session cookie and traded for a full session — and,
// because clientID rides in `jti`, a session belonging to the victim's client
// identity.
func TestVerifyJWT_RejectsWTTicket(t *testing.T) {
	ticket, err := auth.IssueWTTicket(testSecret, "victim-client")
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the ticket really is valid *as a ticket*, so a failure below is
	// about the subject check and not about an unrelated broken token.
	if _, ok := auth.VerifyWTTicket(testSecret, ticket); !ok {
		t.Fatal("fixture is wrong: the wt-ticket does not verify as a ticket")
	}
	if clientID, ok := auth.VerifyJWT(testSecret, ticket); ok {
		t.Fatalf("a wt-ticket verified as a session JWT (clientID=%q); sub must be checked", clientID)
	}
}

// TestVerifyJWT_AcceptsIssuedSession is the other half of the guard: the fix
// must not invalidate any live session. IssueJWT has always written
// sub=tether, so requiring it is a one-directional tightening.
func TestVerifyJWT_AcceptsIssuedSession(t *testing.T) {
	tok, err := auth.IssueJWT(testSecret, "client-abc")
	if err != nil {
		t.Fatal(err)
	}
	clientID, ok := auth.VerifyJWT(testSecret, tok)
	if !ok {
		t.Fatal("a token from IssueJWT must still verify — existing sessions must not be cut off")
	}
	if clientID != "client-abc" {
		t.Fatalf("clientID = %q, want %q", clientID, "client-abc")
	}
}

// TestMiddleware_WTTicketCookie_BuysNoSession is the end-to-end form of A2: the
// full attack, through the real middleware. A stolen ticket in the session
// cookie must produce 401 AND no renewed cookie — a Set-Cookie here would mean
// the 60-second ticket had been laundered into a 90-day session.
func TestMiddleware_WTTicketCookie_BuysNoSession(t *testing.T) {
	s := auth.NewState("tok", testSecret)
	ticket, err := auth.IssueWTTicket(testSecret, "victim-client")
	if err != nil {
		t.Fatal(err)
	}

	innerCalled := false
	h := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: ticket})
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wt-ticket as session cookie: got %d, want 401", rr.Code)
	}
	if innerCalled {
		t.Fatal("inner handler reached with a wt-ticket in the session cookie")
	}
	for _, sc := range rr.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, auth.CookieName+"=") && !strings.Contains(sc, "Max-Age=0") {
			t.Fatalf("a wt-ticket was traded for a renewed session cookie: %q", sc)
		}
	}
}

// TestMiddleware_SessionCookie_StillRenews pins the no-regression half at the
// middleware level: a real session cookie keeps working and keeps sliding.
func TestMiddleware_SessionCookie_StillRenews(t *testing.T) {
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
	req := httptest.NewRequest("GET", "/api/v1/sessions", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: tok})
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || !innerCalled {
		t.Fatalf("valid session cookie: code=%d innerCalled=%v, want 200/true", rr.Code, innerCalled)
	}
	renewed := false
	for _, sc := range rr.Result().Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, auth.CookieName+"=") {
			renewed = true
		}
	}
	if !renewed {
		t.Error("valid session cookie should be renewed (sliding expiry)")
	}
}
