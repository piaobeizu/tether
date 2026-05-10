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

func (s *State) isExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/auth",
		p == "/api/v1/auth/verify",
		p == "/",
		hasSuffix(p, ".js", ".css", ".ico", ".png", ".svg", ".woff2", ".woff", ".ttf", ".webmanifest"):
		return true
	}
	return false
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
