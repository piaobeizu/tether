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

// Handlers provides the three OAuth 2.1 endpoint handlers.
type Handlers struct {
	codes  *CodeStore
	tokens *apitoken.Store
	issuer string
	log    *slog.Logger
}

// NewHandlers creates Handlers. issuer must be https://<host>[:<port>].
func NewHandlers(codes *CodeStore, tokens *apitoken.Store, issuer string) *Handlers {
	return &Handlers{codes: codes, tokens: tokens, issuer: issuer, log: slog.Default()}
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
	return h == "localhost" || h == "127.0.0.1"
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
	if h := r.Header.Get("X-Real-IP"); h != "" {
		return h
	}
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
