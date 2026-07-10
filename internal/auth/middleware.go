package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// State holds the per-daemon auth config.
type State struct {
	token  string
	secret []byte
}

// NewState initialises auth from pre-loaded token and secret.
func NewState(token string, secret []byte) *State {
	return &State{token: token, secret: secret}
}

// Middleware wraps h, requiring a valid tether_session cookie on non-exempt routes.
func (s *State) Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.isExempt(r) {
			h.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(CookieName)
		clientID, valid := "", false
		if err == nil {
			clientID, valid = VerifyJWT(s.secret, c.Value)
		}
		if !valid {
			if isAPIorWT(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			} else {
				http.Redirect(w, r, "/auth", http.StatusFound)
			}
			return
		}
		// Sliding renewal: reissue JWT with same clientID to reset Max-Age.
		if tok, err := IssueJWT(s.secret, clientID); err == nil {
			w.Header().Add("Set-Cookie", FormatSetCookie(tok))
		} else {
			slog.Warn("auth: JWT renewal failed", "err", err)
		}
		h.ServeHTTP(w, r)
	})
}

// VerifyHandler handles POST /api/v1/auth/verify.
func (s *State) VerifyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		Token    string `json:"token"`
		ClientID string `json:"clientId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	clientID := body.ClientID
	if len(clientID) > 64 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if clientID == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		clientID = fmt.Sprintf("%x", b)
	}
	tok, err := IssueJWT(s.secret, clientID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Add("Set-Cookie", FormatSetCookie(tok))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// LogoutHandler handles GET /api/v1/auth/logout — clears the session cookie
// and redirects to /auth. Clearing an HttpOnly cookie requires server-side
// Set-Cookie with Max-Age=0; JS cannot do this.
func (s *State) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Set-Cookie", fmt.Sprintf(
		"%s=; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=0",
		CookieName,
	))
	http.Redirect(w, r, "/auth", http.StatusFound)
}

// ClientIDFromRequest extracts the client identity (JWT jti) from the
// tether_session cookie. Returns "" if the cookie is absent or invalid.
// Use this instead of trusting a ?clientId= URL param — the cookie has
// already been HMAC-verified by the auth middleware.
func (s *State) ClientIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	clientID, ok := VerifyJWT(s.secret, c.Value)
	if !ok {
		return ""
	}
	return clientID
}

// WtTicketHandler handles POST /api/v1/auth/wt-ticket.
// Requires a valid tether_session cookie (enforced by auth middleware).
// Issues a short-lived (60s) JWT for one WebTransport connection; the
// ticket is passed as ?ticket= in the WT URL because Chrome's WT CONNECT
// does not carry cookies.
func (s *State) WtTicketHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	clientID := s.ClientIDFromRequest(r)
	if clientID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		return
	}
	ticket, err := IssueWTTicket(s.secret, clientID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"ticket": ticket})
}

// ClientIDFromTicket extracts client identity from a ?ticket= WT ticket.
// Returns "" if the ticket is absent, invalid, or expired.
// Used by WebTransport handlers where cookies are unavailable.
func (s *State) ClientIDFromTicket(r *http.Request) string {
	ticket := r.URL.Query().Get("ticket")
	if ticket == "" {
		return ""
	}
	clientID, ok := VerifyWTTicket(s.secret, ticket)
	if !ok {
		return ""
	}
	return clientID
}

func (s *State) isExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/auth",
		p == "/api/v1/auth/verify",
		p == "/api/v1/auth/logout",
		p == "/cert-hash",
		p == "/cert-hash-spki",
		p == "/mcp",
		strings.HasPrefix(p, "/mcp/"),
		// WebTransport CONNECT uses a new QUIC connection that may not carry
		// SameSite=Strict cookies. Auth is checked inside the handler instead.
		strings.HasPrefix(p, "/wt/"),
		p == "/oauth/authorize",
		p == "/oauth/token",
		p == "/.well-known/oauth-authorization-server":
		return true
	// Static-asset suffixes (.js/.css/fonts/favicon/manifest) are exempt from
	// the cookie check ONLY outside protected namespaces. The SPA and all its
	// assets are served by the catch-all "/" handler (embed.FS) — never under
	// /api, /wt, or /mcp. Gating on the suffix alone let a client-controlled
	// trailing path segment (e.g. /api/v1/work/items/x.js) masquerade as a
	// static asset and bypass cookie auth, reaching a handler that calls aihub
	// with the daemon's server-side key (tether#16).
	case !isProtectedNamespace(p) &&
		hasSuffix(p, ".js", ".css", ".ico", ".png", ".svg", ".woff2", ".woff", ".ttf", ".webmanifest"):
		return true
	}
	return false
}

// isProtectedNamespace reports whether p lives under a namespace that must
// always be authenticated (cookie in the middleware, or a ticket/session
// inside the handler for /wt and /mcp). Static-asset suffix exemption must
// never apply to these, since their trailing path segments are client-controlled.
func isProtectedNamespace(p string) bool {
	return strings.HasPrefix(p, "/api/") ||
		strings.HasPrefix(p, "/wt/") ||
		p == "/mcp" || strings.HasPrefix(p, "/mcp/")
}

// IsExemptForTest exposes isExempt for whitebox testing.
func (s *State) IsExemptForTest(r *http.Request) bool {
	return s.isExempt(r)
}

func isAPIorWT(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/wt/")
}

func hasSuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
