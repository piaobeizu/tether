package auth

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeEnv builds an envLookup backed by a fixed map, mirroring os.LookupEnv's
// (value, ok) contract without touching process-wide environment state. This
// is the injectable seam loadOrGenTokenFrom takes in place of os.LookupEnv,
// mirroring the envLookup pattern in internal/aihub/config.go.
func fakeEnv(m map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

// noEnv reports every lookup as unset, as if TETHER_TOKEN were never present
// in the environment.
func noEnv(string) (string, bool) { return "", false }

// tokenFilePath returns a path to a not-yet-existing token file inside a
// fresh t.TempDir(), so tests never read or write the real
// ~/.tether/access-token.
func tokenFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "access-token")
}

// at wraps a fixed path as the pathFn loadOrGenTokenFrom takes. The lazy
// signature exists so the flag/env branches never resolve (and never create)
// the token directory; tests assert that laziness via failPath below.
func at(p string) func() (string, error) {
	return func() (string, error) { return p, nil }
}

// failPath is a pathFn that fails, standing in for "HOME is unavailable or
// not writable". Using it proves a branch never reached the file at all.
func failPath() (string, error) {
	return "", errors.New("token path unavailable")
}

func TestLoadOrGenTokenFrom_FlagOnly(t *testing.T) {
	path := tokenFilePath(t)

	token, src, err := loadOrGenTokenFrom("abc123", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "abc123" {
		t.Errorf("token = %q, want %q", token, "abc123")
	}
	if src != TokenSourceFlag {
		t.Errorf("source = %q, want %q", src, TokenSourceFlag)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("flag-supplied token must not touch the token file, but %s exists", path)
	}
}

func TestLoadOrGenTokenFrom_FlagNotTrimmed(t *testing.T) {
	// Documents that today's exact semantics are preserved: only the empty
	// string falls through. A flag value of a single space is used verbatim.
	path := tokenFilePath(t)

	token, src, err := loadOrGenTokenFrom(" ", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != " " {
		t.Errorf("token = %q, want %q (untrimmed)", token, " ")
	}
	if src != TokenSourceFlag {
		t.Errorf("source = %q, want %q", src, TokenSourceFlag)
	}
}

func TestLoadOrGenTokenFrom_EnvOnly(t *testing.T) {
	path := tokenFilePath(t)
	env := fakeEnv(map[string]string{EnvTokenVar: "env-token-value"})

	token, src, err := loadOrGenTokenFrom("", env, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "env-token-value" {
		t.Errorf("token = %q, want %q", token, "env-token-value")
	}
	if src != TokenSourceEnv {
		t.Errorf("source = %q, want %q", src, TokenSourceEnv)
	}
}

func TestLoadOrGenTokenFrom_FlagBeatsEnv(t *testing.T) {
	// The precedence decision under test: flag wins over env even when both
	// are set. Getting this backwards produces "I passed --token and it was
	// silently ignored".
	path := tokenFilePath(t)
	env := fakeEnv(map[string]string{EnvTokenVar: "env-token-value"})

	token, src, err := loadOrGenTokenFrom("flag-token-value", env, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "flag-token-value" {
		t.Errorf("token = %q, want %q (flag must win)", token, "flag-token-value")
	}
	if src != TokenSourceFlag {
		t.Errorf("source = %q, want %q", src, TokenSourceFlag)
	}
}

func TestLoadOrGenTokenFrom_EnvWhitespaceOnlyFallsThroughToFile(t *testing.T) {
	path := tokenFilePath(t)
	if err := os.WriteFile(path, []byte("file-token-value\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	env := fakeEnv(map[string]string{EnvTokenVar: "   "})

	token, src, err := loadOrGenTokenFrom("", env, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "file-token-value" {
		t.Errorf("token = %q, want %q (whitespace-only env should fall through)", token, "file-token-value")
	}
	if src != TokenSourceFile {
		t.Errorf("source = %q, want %q", src, TokenSourceFile)
	}
}

func TestLoadOrGenTokenFrom_EnvEmptyFallsThroughToFile(t *testing.T) {
	path := tokenFilePath(t)
	if err := os.WriteFile(path, []byte("file-token-value\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	// Explicitly present but empty, as os.LookupEnv reports for `TETHER_TOKEN=`.
	env := fakeEnv(map[string]string{EnvTokenVar: ""})

	token, src, err := loadOrGenTokenFrom("", env, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "file-token-value" {
		t.Errorf("token = %q, want %q (empty env should fall through)", token, "file-token-value")
	}
	if src != TokenSourceFile {
		t.Errorf("source = %q, want %q", src, TokenSourceFile)
	}
}

func TestLoadOrGenTokenFrom_EnvTrimmed(t *testing.T) {
	// A trailing space in a systemd EnvironmentFile must not silently mint a
	// token that looks right but always fails auth.
	path := tokenFilePath(t)
	env := fakeEnv(map[string]string{EnvTokenVar: "\t  env-token-value  \t"})

	token, src, err := loadOrGenTokenFrom("", env, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "env-token-value" {
		t.Errorf("token = %q, want exactly trimmed %q", token, "env-token-value")
	}
	if src != TokenSourceEnv {
		t.Errorf("source = %q, want %q", src, TokenSourceEnv)
	}
}

func TestLoadOrGenTokenFrom_FileOnly(t *testing.T) {
	path := tokenFilePath(t)
	if err := os.WriteFile(path, []byte("file-token-value\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	token, src, err := loadOrGenTokenFrom("", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "file-token-value" {
		t.Errorf("token = %q, want %q", token, "file-token-value")
	}
	if src != TokenSourceFile {
		t.Errorf("source = %q, want %q", src, TokenSourceFile)
	}
}

func TestLoadOrGenTokenFrom_FileEmptyFallsThroughToGeneration(t *testing.T) {
	path := tokenFilePath(t)
	if err := os.WriteFile(path, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	token, src, err := loadOrGenTokenFrom("", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != TokenSourceGenerated {
		t.Errorf("source = %q, want %q", src, TokenSourceGenerated)
	}
	if len(token) != 64 {
		t.Errorf("generated token length = %d, want 64", len(token))
	}
	if _, decErr := hex.DecodeString(token); decErr != nil {
		t.Errorf("generated token %q is not valid hex: %v", token, decErr)
	}
}

func TestLoadOrGenTokenFrom_GeneratesAndPersists(t *testing.T) {
	path := tokenFilePath(t)

	token, src, err := loadOrGenTokenFrom("", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != TokenSourceGenerated {
		t.Errorf("source = %q, want %q", src, TokenSourceGenerated)
	}
	if len(token) != 64 {
		t.Errorf("generated token length = %d, want 64 hex chars", len(token))
	}
	if _, decErr := hex.DecodeString(token); decErr != nil {
		t.Errorf("generated token %q is not valid hex: %v", token, decErr)
	}

	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("expected token file to be written: %v", statErr)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want %o", perm, 0o600)
	}

	// A second call with no flag/env must now find the persisted file and
	// return the SAME token, this time sourced from the file.
	token2, src2, err := loadOrGenTokenFrom("", noEnv, at(path))
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if token2 != token {
		t.Errorf("second call token = %q, want same as first %q", token2, token)
	}
	if src2 != TokenSourceFile {
		t.Errorf("second call source = %q, want %q", src2, TokenSourceFile)
	}
}

// The flag and env branches must resolve without ever touching the token
// file. failPath makes any attempt to reach the filesystem an outright
// error, so a pass here is positive proof of that laziness rather than an
// inference from a missing file. This pins a local invariant (an explicitly
// supplied secret causes no filesystem side effect); it is NOT a claim that
// the daemon runs with an unwritable HOME — server.Run needs one anyway.
func TestLoadOrGenTokenFrom_FlagDoesNotResolvePath(t *testing.T) {
	token, src, err := loadOrGenTokenFrom("flag-token-value", noEnv, failPath)
	if err != nil {
		t.Fatalf("flag branch must not depend on the token path: %v", err)
	}
	if token != "flag-token-value" || src != TokenSourceFlag {
		t.Errorf("got (%q, %q), want (%q, %q)", token, src, "flag-token-value", TokenSourceFlag)
	}
}

func TestLoadOrGenTokenFrom_EnvDoesNotResolvePath(t *testing.T) {
	env := fakeEnv(map[string]string{EnvTokenVar: "env-token-value"})

	token, src, err := loadOrGenTokenFrom("", env, failPath)
	if err != nil {
		t.Fatalf("env branch must not depend on the token path: %v", err)
	}
	if token != "env-token-value" || src != TokenSourceEnv {
		t.Errorf("got (%q, %q), want (%q, %q)", token, src, "env-token-value", TokenSourceEnv)
	}
}

// Conversely, once no explicit secret was supplied the file IS required, so
// a failing pathFn must surface as an error rather than a silent empty token.
func TestLoadOrGenTokenFrom_PathErrorSurfacesWhenFileNeeded(t *testing.T) {
	token, src, err := loadOrGenTokenFrom("", noEnv, failPath)
	if err == nil {
		t.Fatalf("expected an error when the token path is unavailable, got (%q, %q)", token, src)
	}
	if token != "" || src != "" {
		t.Errorf("on error want empty token and source, got (%q, %q)", token, src)
	}
}

// The env var name is load-bearing outside Go: README.md, the systemd unit
// template and deploy/tether.env.example all hard-code the literal string.
// Every other test keys off the constant, so renaming its value would leave
// the suite green while silently breaking the whole deployment story.
func TestEnvTokenVarLiteral(t *testing.T) {
	if EnvTokenVar != "TETHER_TOKEN" {
		t.Fatalf("EnvTokenVar = %q, want %q — docs and deploy/tether.service hard-code this name", EnvTokenVar, "TETHER_TOKEN")
	}
}

// The daemon hands a verbatim os.Environ() to the coding agent and to every
// shell-pane command, so the token must not survive in the environment after
// resolution. Asserted for the env path and, separately, for the flag path —
// where the variable is present but loses, and would otherwise be forgotten.
func TestLoadOrGenToken_ScrubsEnvAfterResolution(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     TokenSource
	}{
		{name: "env path", override: "", want: TokenSourceEnv},
		{name: "flag path, env set but loses", override: "flag-token-value", want: TokenSourceFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Isolate HOME: this exercises the real LoadOrGenToken, and a
			// future precedence change must not be able to read or write the
			// developer's actual ~/.tether/access-token.
			t.Setenv("HOME", t.TempDir())
			t.Setenv(EnvTokenVar, "env-token-value")

			_, src, err := LoadOrGenToken(tc.override)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if src != tc.want {
				t.Fatalf("source = %q, want %q", src, tc.want)
			}
			if v, ok := os.LookupEnv(EnvTokenVar); ok {
				t.Errorf("%s still set after resolution (=%q) — it would be inherited by agent and shell subprocesses", EnvTokenVar, v)
			}
		})
	}
}
