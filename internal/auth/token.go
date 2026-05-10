package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrGenToken returns the access token. If override is non-empty it is used
// at runtime only (NOT persisted to disk). Otherwise ~/.tether/access-token is
// read or generated on first run.
func LoadOrGenToken(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	path, err := tokenPath()
	if err != nil {
		return "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(data))
		if t != "" {
			return t, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write token: %w", err)
	}
	// Print to stdout so the user sees it once. Redirect stdout to suppress.
	// Do NOT use slog — structured logs may be persisted and expose the secret.
	fmt.Printf("tether access token: %s\n", token)
	fmt.Printf("(stored at %s)\n", path)
	return token, nil
}

func tokenPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "access-token"), nil
}
