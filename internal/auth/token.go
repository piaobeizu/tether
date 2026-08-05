package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenSource identifies which of the four precedence levels supplied the
// access token. Callers should log this label — never the token value — to
// confirm which credential path is actually energised.
type TokenSource string

const (
	// TokenSourceFlag means the --token CLI flag (override) was non-empty.
	TokenSourceFlag TokenSource = "flag"
	// TokenSourceEnv means the TETHER_TOKEN environment variable was set to a
	// non-blank value.
	TokenSourceEnv TokenSource = "env"
	// TokenSourceFile means the token was read from the on-disk token file.
	TokenSourceFile TokenSource = "file"
	// TokenSourceGenerated means no source provided a token, so a new one was
	// generated and persisted.
	TokenSourceGenerated TokenSource = "generated"
)

// EnvTokenVar is the environment variable consulted for the access token
// (precedence level 2). Exported so callers, docs, and tests share a single
// source of truth for the name.
const EnvTokenVar = "TETHER_TOKEN"

// envLookup abstracts environment variable lookup so loadOrGenTokenFrom's
// resolution logic can be exercised in tests without mutating process-wide
// env state. Mirrors the pattern in internal/aihub/config.go.
type envLookup func(string) (string, bool)

// LoadOrGenToken returns the access token and the source it came from, using
// a four-level precedence:
//
//  1. flag: override is non-empty (the --token CLI flag). Used at runtime
//     only — never persisted to disk. Not trimmed: only the empty string
//     falls through, preserving today's exact semantics.
//  2. env: the TETHER_TOKEN environment variable, trimmed, if non-blank
//     after trimming. Flag beats env deliberately — this is specificity,
//     not security: it matches the universal flag > env > file > default
//     convention, and lets an operator run `tether server --token X` for a
//     one-off debug session without editing the unit's EnvironmentFile. The
//     inverse ordering would produce "I passed --token and it was silently
//     ignored", the hardest kind of failure to diagnose. The value is
//     trimmed (unlike the flag) because a trailing space in a systemd
//     EnvironmentFile would otherwise create a token that looks correct and
//     always fails auth.
//  3. file: ~/.tether/access-token, if it exists and is non-blank after
//     TrimSpace.
//  4. generated: otherwise a new token is generated, persisted to
//     ~/.tether/access-token with mode 0600, and printed once to stdout.
//
// LoadOrGenToken also SCRUBS TETHER_TOKEN from the process environment before
// returning, whichever source won. See scrubTokenEnv for why.
func LoadOrGenToken(override string) (string, TokenSource, error) {
	tok, src, err := loadOrGenTokenFrom(override, os.LookupEnv, tokenPath)
	scrubTokenEnv()
	return tok, src, err
}

// scrubTokenEnv removes TETHER_TOKEN from the process environment.
//
// This is not hygiene, it is containment. tether spawns the coding agent and
// every shell-pane command with a verbatim copy of os.Environ(), so anything
// left in the daemon's environment is handed to a process driven by an LLM —
// one `env` dump away from a chat transcript that leaves the machine and is
// persisted in plaintext. The daemon's own master credential has no business
// being there: it has already been read into memory by the time this runs, and
// nothing downstream re-reads the variable.
//
// It runs unconditionally rather than only on the env path: if an operator
// passes --token while TETHER_TOKEN also happens to be exported, the flag wins
// for authentication but the environment copy would still leak.
//
// Call site placement matters. This must run before any subprocess that
// inherits an uncurated environment is spawned. Run() resolves the token at
// step 4c, ahead of every agent/PTY spawn (which are per-session and later),
// so scrubbing here is early enough. Task-scoped MCP servers use a curated
// allowlist that a workspace's own task-config.json can extend, which is
// precisely why the variable must be gone rather than merely unused.
func scrubTokenEnv() {
	_ = os.Unsetenv(EnvTokenVar)
}

// loadOrGenTokenFrom is the testable core of LoadOrGenToken: env is an
// injectable lookup function (normally os.LookupEnv) and pathFn resolves the
// on-disk token file. It must not call os.UserHomeDir itself.
//
// pathFn is a function rather than a plain string so that it is only invoked
// when the file is actually needed, preserving the pre-existing property that
// an explicitly supplied --token never touches the filesystem: tokenPath() has
// the side effect of MkdirAll-ing ~/.tether. This is a local invariant only —
// it does NOT mean the daemon tolerates an unwritable HOME, which it does not
// (server.Run creates ~/.tether at startup and writes the cert, the JWT
// secret, and the hook binary there regardless of where the token came from).
func loadOrGenTokenFrom(override string, env envLookup, pathFn func() (string, error)) (string, TokenSource, error) {
	if override != "" {
		return override, TokenSourceFlag, nil
	}
	if v, ok := env(EnvTokenVar); ok {
		if t := strings.TrimSpace(v); t != "" {
			return t, TokenSourceEnv, nil
		}
	}
	path, err := pathFn()
	if err != nil {
		return "", "", err
	}
	if data, err := os.ReadFile(path); err == nil {
		t := strings.TrimSpace(string(data))
		if t != "" {
			return t, TokenSourceFile, nil
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	token := hex.EncodeToString(b)
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", "", fmt.Errorf("write token: %w", err)
	}
	// Print to stdout so the user sees it once. Redirect stdout to suppress.
	// Do NOT use slog — structured logs may be persisted and expose the secret.
	fmt.Printf("tether access token: %s\n", token)
	fmt.Printf("(stored at %s)\n", path)
	return token, TokenSourceGenerated, nil
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
