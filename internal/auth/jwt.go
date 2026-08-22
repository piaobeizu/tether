package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	cookieName   = "tether_session"
	cookieMaxAge = 90 * 24 * 60 * 60 // 90 days in seconds
	CookieName   = cookieName
	CookieMaxAge = cookieMaxAge
	jwtTTL       = 90 * 24 * time.Hour
	WtTicketTTL  = 60 * time.Second

	// subjSession and subjWTTicket are the `sub` claims that separate the two
	// token kinds. They are signed with the SAME secret by the SAME algorithm,
	// so `sub` is the only thing that distinguishes a 90-day session cookie from
	// a 60-second WebTransport ticket — and the ticket travels in a URL query
	// param (?ticket=), which means proxy logs, browser history and Referer
	// headers. Both verifiers must check it, in both directions (tether#117 A2).
	subjSession  = "tether"
	subjWTTicket = "wt-ticket"
)

// LoadOrGenSecret returns the HMAC signing secret from ~/.tether/jwt-secret,
// generating and persisting it on first run.
func LoadOrGenSecret() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	path := filepath.Join(dir, "jwt-secret")
	if data, err := os.ReadFile(path); err == nil {
		b, decErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decErr == nil && len(b) == 32 {
			return b, nil
		}
		slog.Warn("jwt-secret file corrupt or wrong length, regenerating", "path", path)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write jwt-secret: %w", err)
	}
	return secret, nil
}

// IssueJWT creates a signed JWT with exp + jti (clientID) claims.
// clientID is a browser-generated UUID used as durable client identity.
func IssueJWT(secret []byte, clientID string) (string, error) {
	now := time.Now()
	header := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"sub": subjSession,
		"iat": now.Unix(),
		"exp": now.Add(jwtTTL).Unix(),
		"jti": clientID,
	})
	if err != nil {
		return "", fmt.Errorf("jwt payload: %w", err)
	}
	body := header + "." + base64url(payload)
	sig := hmacSHA256(secret, body)
	return body + "." + sig, nil
}

// VerifyJWT returns (clientID, true) if the token is a session JWT with a valid
// signature that has not expired.
//
// It rejects any token whose sub is not subjSession, which is what stops a
// WebTransport ticket from being spent as a session (tether#117 A2). Before that
// check the claims struct did not even have a sub field, so ANY unexpired token
// signed with the daemon secret verified here — including the 60-second ticket
// that rides in a ?ticket= URL param. Pasted into the tether_session cookie it
// bought a 90-day session, and since clientID is carried in jti, one under the
// victim's client identity (which is what session ownership is decided on).
//
// Requiring the claim invalidates nothing: IssueJWT has always written
// sub=tether, so this only adds the missing direction of a guard
// VerifyWTTicket already had.
func VerifyJWT(secret []byte, token string) (clientID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	body := parts[0] + "." + parts[1]
	expected := hmacSHA256(secret, body)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", false
	}
	if claims.Sub != subjSession {
		return "", false
	}
	if time.Now().Unix() > claims.Exp {
		return "", false
	}
	return claims.Jti, true
}

// FormatSetCookie returns the Set-Cookie header value for a new tether_session JWT.
func FormatSetCookie(token string) string {
	return fmt.Sprintf(
		"%s=%s; HttpOnly; Secure; SameSite=Strict; Path=/; Max-Age=%d",
		cookieName, token, cookieMaxAge,
	)
}

// IssueWTTicket issues a short-lived JWT (60s) for a single WebTransport
// connection. The ticket carries the same clientID as the session JWT but is
// safe to pass in a URL query param because it expires quickly.
func IssueWTTicket(secret []byte, clientID string) (string, error) {
	now := time.Now()
	header := base64url([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]any{
		"sub": subjWTTicket,
		"iat": now.Unix(),
		"exp": now.Add(WtTicketTTL).Unix(),
		"jti": clientID,
	})
	if err != nil {
		return "", fmt.Errorf("wt-ticket payload: %w", err)
	}
	body := header + "." + base64url(payload)
	sig := hmacSHA256(secret, body)
	return body + "." + sig, nil
}

// VerifyWTTicket validates a WT ticket token, returning (clientID, true) on success.
// It rejects tokens whose sub is not "wt-ticket" so session JWTs cannot be reused.
func VerifyWTTicket(secret []byte, token string) (clientID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	body := parts[0] + "." + parts[1]
	expected := hmacSHA256(secret, body)
	if !hmac.Equal([]byte(expected), []byte(parts[2])) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return "", false
	}
	if claims.Sub != subjWTTicket {
		return "", false
	}
	if time.Now().Unix() > claims.Exp {
		return "", false
	}
	return claims.Jti, true
}

func hmacSHA256(secret []byte, data string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(data))
	return base64url(mac.Sum(nil))
}

func base64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}
