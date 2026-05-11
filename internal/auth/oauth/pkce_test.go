package oauth_test

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"

	"github.com/piaobeizu/tether/internal/auth/oauth"
)

func TestVerifyS256_Valid(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if !oauth.VerifyS256(verifier, challenge) {
		t.Error("expected true for correct S256 pair")
	}
}

func TestVerifyS256_WrongVerifier(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	if oauth.VerifyS256("wrong-verifier", challenge) {
		t.Error("expected false for wrong verifier")
	}
}

func TestVerifyS256_EmptyInputs(t *testing.T) {
	if oauth.VerifyS256("", "anything") {
		t.Error("empty verifier should return false")
	}
	if oauth.VerifyS256("anything", "") {
		t.Error("empty challenge should return false")
	}
}
