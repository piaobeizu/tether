// Package aihub provides a minimal read-only HTTP client for the polyforge
// aihub backend, plus credential loading for the tether workbench.
package aihub

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// envLookup abstracts environment variable lookup so LoadConfig's resolution
// logic can be exercised in tests without mutating process-wide env state.
type envLookup func(string) (string, bool)

// LoadConfig resolves the aihub base URL and API key.
//
// Resolution order:
//  1. Environment variables TETHER_AIHUB_URL / TETHER_AIHUB_KEY.
//  2. Best-effort fallback to ~/.polyforge/config.toml ([server].url and
//     [auth].api_key) for whichever value the environment didn't provide.
//
// ok is false if neither source yields both a non-empty url and key. This
// function never returns an error and never panics — a missing or malformed
// config.toml is treated the same as "no fallback available".
func LoadConfig() (baseURL string, key string, ok bool) {
	tomlPath := ""
	if home, err := os.UserHomeDir(); err == nil {
		tomlPath = filepath.Join(home, ".polyforge", "config.toml")
	}
	return loadConfigFrom(os.LookupEnv, tomlPath)
}

// loadConfigFrom is the testable core of LoadConfig: env is an injectable
// lookup function (normally os.LookupEnv) and tomlPath points at a
// polyforge config.toml, which may not exist.
func loadConfigFrom(env envLookup, tomlPath string) (baseURL string, key string, ok bool) {
	envURL, urlOK := env("TETHER_AIHUB_URL")
	envKey, keyOK := env("TETHER_AIHUB_KEY")

	if urlOK && keyOK && envURL != "" && envKey != "" {
		return envURL, envKey, true
	}

	// At least one of url/key is missing from the environment; best-effort
	// fill the gap(s) from config.toml.
	tomlURL, tomlKey := parseTomlConfig(tomlPath)

	baseURL = envURL
	if baseURL == "" {
		baseURL = tomlURL
	}
	key = envKey
	if key == "" {
		key = tomlKey
	}

	if baseURL == "" || key == "" {
		return "", "", false
	}
	return baseURL, key, true
}

// parseTomlConfig hand-parses just the two keys tether needs from a
// polyforge config.toml: [server].url and [auth].api_key. It intentionally
// avoids pulling in a TOML dependency for two scalar values.
//
// It tolerates a missing/unreadable file by returning empty strings — never
// an error, never a panic.
func parseTomlConfig(path string) (url string, apiKey string) {
	if path == "" {
		return "", ""
	}
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"`)

		switch {
		case section == "server" && k == "url":
			url = v
		case section == "auth" && k == "api_key":
			apiKey = v
		}
	}
	return url, apiKey
}
