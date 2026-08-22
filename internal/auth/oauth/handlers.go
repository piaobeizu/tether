package oauth

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/piaobeizu/tether/internal/auth/apitoken"
)

// SessionAuth reports whether r carries an authenticated tether browser
// session. The daemon wires this to the auth middleware's cookie verifier
// (auth.State.ClientIDFromRequest); it is a function rather than a concrete
// type so this package does not have to import internal/auth. Being the same
// verifier the middleware uses is the point — a second, parallel notion of
// "signed in" here would drift from the one the middleware enforces.
//
// It gates the consent POST, which is the mint, and it decides which page the
// consent GET renders (tether#153). A nil SessionAuth denies every consent POST
// and shows every GET the sign-in prompt, which is the safe direction: an
// embedder that forgot to wire it gets a broken approval flow, not a public
// token mint.
type SessionAuth func(*http.Request) bool

// Handlers provides the three OAuth 2.1 endpoint handlers.
type Handlers struct {
	codes   *CodeStore
	tokens  *apitoken.Store
	issuer  string
	session SessionAuth
	log     *slog.Logger
}

// NewHandlers creates Handlers. issuer must be https://<host>[:<port>].
// session gates POST /oauth/authorize; see SessionAuth.
func NewHandlers(codes *CodeStore, tokens *apitoken.Store, issuer string, session SessionAuth) *Handlers {
	return &Handlers{codes: codes, tokens: tokens, issuer: issuer, session: session, log: slog.Default()}
}

// Metadata returns the /.well-known/oauth-authorization-server handler (RFC 8414).
func (h *Handlers) Metadata() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                h.issuer,
			"authorization_endpoint":                h.issuer + "/oauth/authorize",
			"token_endpoint":                        h.issuer + "/oauth/token",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"none"},
			"scopes_supported":                      []string{"mcp"},
		})
	})
}

var approvalTmpl = template.Must(template.New("approval").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8">
<title>tether — Authorize App</title>
<style>body{font-family:sans-serif;max-width:480px;margin:4rem auto;padding:1rem}
button{margin-right:1rem;padding:.5rem 1.5rem;font-size:1rem;cursor:pointer}</style>
</head><body>
<h2>tether — Authorize App</h2>
<p><strong>{{.ClientID}}</strong> is requesting access to tether.</p>
<p>Scope: <code>{{.Scope}}</code></p>
<form method="POST" action="/oauth/authorize">
  <input type="hidden" name="req_id" value="{{.ReqID}}">
  <button name="action" value="allow" type="submit">Allow</button>
  <button name="action" value="deny" type="submit">Deny</button>
</form>
</body></html>`))

// signInTmpl is what a signed-out browser gets from GET /oauth/authorize instead
// of the consent page (tether#153).
//
// GET stays exempt from the auth middleware — this does not change whether the
// endpoint is reachable without a cookie, only what a cookie-less browser is
// shown once it arrives. (internal/auth/middleware.go says not to widen that
// exemption back to every method; nothing here does.)
//
// Rendering consent to a signed-out browser was a dead end. tether#117 A1 put a
// session gate on the POST, so the owner clicked Allow and got a 401; the
// middleware then sent that non-API request to /auth carrying NO ?redirect=, and
// they landed on / with the pending request lost. From the outside: "I clicked
// approve and nothing happened." The pending record died either way — the consent
// page only made it die one click later — so the fix is to put the sign-in step
// in front of the consent step and carry the original request across it.
//
// The link is the whole feature: {{.AuthURL}} takes the browser to /auth with the
// complete authorization request (path AND query) in ?redirect=, so after signing
// in it comes straight back here with client_id, code_challenge, state and
// redirect_uri intact and the consent page renders for real.
var signInTmpl = template.Must(template.New("signin").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8">
<title>tether — Sign in to continue</title>
<style>body{font-family:sans-serif;max-width:480px;margin:4rem auto;padding:1rem}
a.signin{display:inline-block;margin-top:.5rem;padding:.5rem 1.5rem;font-size:1rem;
border:1px solid #888;border-radius:4px;text-decoration:none}</style>
</head><body>
<h2>tether — Sign in to continue</h2>
<p><strong>{{.ClientID}}</strong> is requesting access to tether, but this browser is not signed in.</p>
<p>Sign in first — you will come straight back to this approval page.</p>
<p><a id="oauth-signin-continue" class="signin" href="{{.AuthURL}}">Sign in and continue</a></p>
</body></html>`))

// authorizePath is the only path this handler ever asks a browser to come back
// to, and it is a constant rather than r.URL.Path on purpose.
//
// The request path is client-controlled, and echoing it into a return target is
// the exact mistake tether#117 A3 made twice: `/..//evil.example` parses to OUR
// origin, because `..` segments collapse during parsing, and leaves behind a
// protocol-relative pathname that navigates off-site. Assembling the target from
// a constant makes that unreachable instead of merely guarded against.
const authorizePath = "/oauth/authorize"

// signInURL builds the /auth?redirect= link that returns the browser to this
// exact authorization request once it has signed in.
//
// The consumer is safeRedirectTarget() in web/src/AuthPage.tsx, and it is a
// validator, not a sanitiser: it hands back "/" unless the decoded target
// resolves to this origin AND begins with exactly one "/". So the target is built
// to satisfy that by construction — constant path first, the raw query appended
// after a "?", the whole thing percent-encoded into a single parameter, which is
// what keeps the inner query from being read as /auth's own.
//
// url.QueryEscape emits only [A-Za-z0-9-_.~%+], so the value carries nothing that
// is significant in an HTML attribute and nothing that could open a second
// parameter; html/template's URL-context escaping is a no-op on it rather than
// something this has to survive.
//
// That argument is not the gate, though. testdata/signin_redirect_corpus.json
// pins the exact strings this produces, and web/src/oauthSignInRedirect.test.ts
// feeds that same file through the real safeRedirectTarget — including the two
// shapes A3 got wrong, `..` segments and same-origin absolute URLs.
func signInURL(rawQuery string) string {
	target := authorizePath
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	return "/auth?redirect=" + url.QueryEscape(target)
}

// Authorize handles GET (show approval page) and POST (allow/deny submit).
func (h *Handlers) Authorize() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			h.authorizeGet(w, r)
		case http.MethodPost:
			h.authorizePost(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (h *Handlers) authorizeGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		http.Error(w, "invalid_request: response_type must be code", http.StatusBadRequest)
		return
	}
	redirectURI := q.Get("redirect_uri")
	if !isLoopback(redirectURI) {
		http.Error(w, "invalid_request: redirect_uri must be loopback (localhost or 127.0.0.1)", http.StatusBadRequest)
		return
	}
	clientID := q.Get("client_id")
	challenge := q.Get("code_challenge")
	state := q.Get("state")
	if challenge == "" {
		oauthRedirectError(w, r, redirectURI, "invalid_request", "code_challenge required", state)
		return
	}
	if q.Get("code_challenge_method") != "S256" {
		oauthRedirectError(w, r, redirectURI, "invalid_request", "only S256 supported", state)
		return
	}
	scope := q.Get("scope")
	if scope != "mcp" {
		scope = "mcp"
	}
	// A signed-out browser gets the sign-in step, not the consent page
	// (tether#153) — see signInTmpl for why the consent page was a dead end.
	//
	// This sits AFTER request validation and BEFORE StorePending, and both halves
	// are deliberate. After validation, because a malformed authorization request
	// is malformed whether or not anyone is signed in, and a login prompt would
	// only replay the same error once the round trip finished. Before
	// StorePending, because a record created here would be an orphan: the browser
	// returns through this same handler and stores a fresh one.
	if h.session == nil || !h.session(r) {
		h.log.Info("oauth.authorize.signin_required", "client_id", clientID, "remote_ip", remoteIP(r))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_ = signInTmpl.Execute(w, struct {
			ClientID string
			AuthURL  string
		}{clientID, signInURL(r.URL.RawQuery)})
		return
	}
	reqID, err := h.codes.StorePending(clientID, redirectURI, challenge, scope, state)
	if err != nil {
		h.log.Error("oauth.authorize.store_failed", "err", err)
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = approvalTmpl.Execute(w, struct {
		ClientID string
		Scope    string
		ReqID    string
	}{clientID, scope, reqID})
}

func (h *Handlers) authorizePost(w http.ResponseWriter, r *http.Request) {
	// Consent must come from an authenticated browser (tether#117 A1).
	//
	// This handler is the mint: it turns a req_id into an authorization code,
	// and /oauth/token turns that code into a 24h Bearer token in the same
	// api-tokens store a hand-issued token lives in. It used to do so for
	// anyone — the auth middleware exempted /oauth/authorize for every method,
	// nothing here looked at a cookie or a client identity, and on a
	// --acme-domain deployment the listener is on the public internet by
	// construction (TLS-ALPN-01 needs :443 reachable from Let's Encrypt). Three
	// curl requests, no credentials.
	//
	// The middleware now exempts GET only, so this is the second lock rather
	// than the only one: a future mux rewiring cannot re-open the mint.
	//
	// BEFORE ConsumePending, deliberately: a rejected caller must not be able to
	// burn the req_id of a consent page the owner is looking at.
	if h.session == nil || !h.session(r) {
		h.log.Warn("oauth.authorize.unauthenticated", "remote_ip", remoteIP(r))
		http.Error(w, "unauthorized: sign in to tether before approving an app", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	reqID := r.FormValue("req_id")
	action := r.FormValue("action")
	ip := remoteIP(r)

	p, err := h.codes.ConsumePending(reqID)
	if err != nil {
		h.log.Warn("oauth.authorize.invalid_req_id", "remote_ip", ip)
		http.Error(w, "invalid or expired authorization request", http.StatusBadRequest)
		return
	}
	if action == "deny" {
		h.log.Info("oauth.authorize.denied", "client_id", p.ClientID, "remote_ip", ip)
		oauthRedirectError(w, r, p.RedirectURI, "access_denied", "", p.State)
		return
	}
	code, err := h.codes.StoreCode(p)
	if err != nil {
		h.log.Error("oauth.authorize.store_code_failed", "err", err)
		http.Error(w, "server_error", http.StatusInternalServerError)
		return
	}
	h.log.Info("oauth.authorize.allowed", "client_id", p.ClientID, "scope", p.Scope, "remote_ip", ip)
	u, _ := url.Parse(p.RedirectURI)
	qv := u.Query()
	qv.Set("code", code)
	if p.State != "" {
		qv.Set("state", p.State)
	}
	u.RawQuery = qv.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// Token handles POST /oauth/token — exchanges an auth code for a Bearer token.
func (h *Handlers) Token() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		if err := r.ParseForm(); err != nil {
			oauthTokenError(w, "invalid_request", "malformed body")
			return
		}
		if r.FormValue("grant_type") != "authorization_code" {
			oauthTokenError(w, "unsupported_grant_type", "only authorization_code is supported")
			return
		}
		clientID := r.FormValue("client_id")
		redirectURI := r.FormValue("redirect_uri")
		verifier := r.FormValue("code_verifier")

		p, err := h.codes.ConsumeCode(r.FormValue("code"))
		if err != nil {
			h.log.Warn("oauth.token.invalid_grant", "reason", "code_not_found", "client_id", clientID)
			oauthTokenError(w, "invalid_grant", "code not found or expired")
			return
		}
		if p.ClientID != clientID {
			h.log.Warn("oauth.token.invalid_grant", "reason", "client_id_mismatch", "client_id", clientID)
			oauthTokenError(w, "invalid_grant", "client_id mismatch")
			return
		}
		if p.RedirectURI != redirectURI {
			h.log.Warn("oauth.token.invalid_grant", "reason", "redirect_uri_mismatch", "client_id", clientID)
			oauthTokenError(w, "invalid_grant", "redirect_uri mismatch")
			return
		}
		if !VerifyS256(verifier, p.Challenge) {
			h.log.Warn("oauth.token.invalid_grant", "reason", "pkce_failure", "client_id", clientID)
			oauthTokenError(w, "invalid_grant", "code_verifier mismatch")
			return
		}
		raw, tok, err := h.tokens.CreateWithTTL(clientID, apitoken.TokenSourceOAuth, 24*time.Hour)
		if err != nil {
			h.log.Error("oauth.token.store_failed", "err", err)
			http.Error(w, "server_error", http.StatusInternalServerError)
			return
		}
		h.log.Info("oauth.token.issued", "client_id", clientID, "token_id", tok.ID, "expires_at", tok.ExpiresAt)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": raw,
			"token_type":   "Bearer",
			"expires_in":   int64(24 * time.Hour / time.Second),
			"scope":        p.Scope,
		})
	})
}

func isLoopback(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func oauthRedirectError(w http.ResponseWriter, r *http.Request, redirectURI, errCode, desc, state string) {
	u, _ := url.Parse(redirectURI)
	q := u.Query()
	q.Set("error", errCode)
	if desc != "" {
		q.Set("error_description", desc)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func oauthTokenError(w http.ResponseWriter, errCode, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             errCode,
		"error_description": desc,
	})
}

func remoteIP(r *http.Request) string {
	addr := r.RemoteAddr
	// IPv6: [::1]:port
	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end > 0 {
			return addr[1:end]
		}
	}
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
