package oauth

import (
	"crypto/sha256"
	"encoding/base64"
)

// VerifyS256 returns true iff BASE64URL(SHA256(verifier)) == challenge.
// Both inputs must be non-empty. Only S256 is supported (plain is rejected
// per OAuth 2.1 §7.5.1).
func VerifyS256(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return got == challenge
}
