package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// State holds the per-daemon auth config.
type State struct {
	token  string
	secret []byte
	// staticFS is the embedded SPA dist (production). When set, static-asset
	// cookie exemption is decided by real-file existence (fail-closed); when
	// nil (dev mode, assets proxied to Vite) a suffix heuristic is used. See
	// isStaticAsset (tether#17).
	staticFS fs.FS
}

// NewState initialises auth from pre-loaded token and secret.
func NewState(token string, secret []byte) *State {
	return &State{token: token, secret: secret}
}

// WithStaticFS attaches the embedded SPA dist filesystem so static-asset
// exemption is decided by real-file existence rather than a suffix heuristic.
// Call only in production (embedded assets); leave unset in dev mode, where the
// SPA is reverse-proxied to Vite and the dist FS does not describe live assets.
// Returns s for chaining.
func (s *State) WithStaticFS(fsys fs.FS) *State {
	s.staticFS = fsys
	return s
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
	// The PreToolUse permission hook (tether-permission-hook) is a loopback
	// subprocess of the daemon-spawned cc; it POSTs the tool request here with
	// NO cookie/ticket (it has no browser session — it routes by session_id in
	// the body). Exempt it — but ONLY for a loopback caller, so a remote client
	// on a public (--acme-domain) deployment can't reach it. EXACT-match /request
	// only: the sibling /decide (approval) stays cookie-gated so only the
	// authenticated browser can approve. Without this the hook got a 401, decoded
	// it as {allow:false}, and every tool was silently denied with no
	// PermissionBlock ever surfacing (tether#39).
	case p == "/api/v1/permission/request" && isLoopbackAddr(r.RemoteAddr):
		return true
	// Static SPA assets are served by the catch-all "/" handler (embed.FS),
	// never under /api, /wt, or /mcp. Exempt them from the cookie check — but
	// fail-closed so a client-controlled trailing segment can't masquerade as
	// an asset and reach a privileged handler with the daemon's server-side
	// key (tether#16 / tether#17). See isStaticAsset.
	case s.isStaticAsset(p):
		return true
	}
	return false
}

// isStaticAsset reports whether p is exempt from the cookie check as a static
// SPA asset. It NEVER exempts a protected namespace (/api,/wt,/mcp). In
// production (staticFS set) a path is exempt only if it resolves to a real
// embedded dist file — so any future non-/api privileged prefix (e.g.
// /admin/x.js) that isn't a shipped asset stays auth-gated (fail-closed),
// closing tether#16's latent fail-open where a suffix match alone sufficed
// (tether#17). In dev (staticFS nil; assets are reverse-proxied to Vite and
// not described by the embedded dist) it falls back to the static-suffix
// heuristic, still gated behind isProtectedNamespace.
func (s *State) isStaticAsset(p string) bool {
	if isProtectedNamespace(p) {
		return false
	}
	if s.staticFS != nil {
		return staticFileExists(s.staticFS, p)
	}
	return hasSuffix(p, ".js", ".css", ".ico", ".png", ".svg", ".woff2", ".woff", ".ttf", ".webmanifest")
}

// staticFileExists reports whether URL path p maps to a regular file in fsys
// (the embedded dist). fs paths are relative, slash-separated, with no leading
// slash and no "." / ".." elements; fs.ValidPath rejects the traversal forms,
// so an attacker can't escape the dist tree.
func staticFileExists(fsys fs.FS, p string) bool {
	name := strings.TrimPrefix(p, "/")
	if name == "" || !fs.ValidPath(name) {
		return false
	}
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
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

// isLoopbackAddr reports whether addr (an http.Request RemoteAddr, "host:port")
// is a loopback peer. Malformed/empty addresses are treated as non-loopback
// (fail-closed), so the permission-request exemption can never widen to a real
// remote client (tether#39). This assumes the daemon terminates its own
// QUIC/TCP listeners (the shipped topology, server.go), so RemoteAddr is the
// real peer address. Do NOT front the daemon with a same-host reverse proxy:
// that would collapse every RemoteAddr to loopback and open the exemption to
// remote callers.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func hasSuffix(s string, suffixes ...string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
