package cc

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// hookEvents lists the cc hook events tether subscribes to. Order is
// stable but not semantically meaningful — settings.json is keyed by
// event name.
var hookEvents = []struct {
	name string // cc canonical EventName (settings.json key)
	path string // URL path under baseURL
}{
	{"PreToolUse", "/hooks/pre-tool-use"},
	{"PostToolUse", "/hooks/post-tool-use"},
	{"SessionStart", "/hooks/session-start"},
	{"UserPromptSubmit", "/hooks/user-prompt-submit"},
	{"Stop", "/hooks/stop"},
}

// HookURLs returns the per-event endpoint URLs the cc settings.json will
// invoke. Exposed for testability — callers verifying end-to-end wiring
// against a live HookServer can use this directly instead of parsing the
// curl command string out of SettingsForCC's output.
//
// Validates baseURL the same way SettingsForCC does (no trailing slash,
// loopback host only).
func HookURLs(baseURL string) (map[string]string, error) {
	if err := validateBaseURL(baseURL); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(hookEvents))
	for _, ev := range hookEvents {
		out[ev.name] = baseURL + ev.path
	}
	return out, nil
}

// validateBaseURL enforces the cc-settings invariants: non-empty,
// no trailing slash (would produce // in hook URLs), loopback host only
// (settings.json is locally-scoped — a remote URL would leak hook events
// off-machine; defense-in-depth against misconfig).
func validateBaseURL(baseURL string) error {
	if baseURL == "" {
		return errors.New("hooks: baseURL required")
	}
	if strings.HasSuffix(baseURL, "/") {
		return errors.New("hooks: baseURL must not have trailing slash (would produce // in hook URL)")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("hooks: parse baseURL: %w", err)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return fmt.Errorf("hooks: baseURL host must be loopback (127.0.0.1 / ::1 / localhost); got %q", host)
	}
	return nil
}

// SettingsForCC produces a cc settings.json document configured to invoke
// the tether hookserver at baseURL for the 5 hook events the daemon cares
// about. The shape matches cc's settings hooks contract: each event maps
// to a list with one matcher whose `hooks` field is a list of {type,
// command}; type=command means cc execs the command, piping the hook
// payload to its stdin and reading the JSON response from its stdout.
//
// We use `curl --data-binary @-` so the bytes round-trip without
// transformation; -sf silences progress and fails non-2xx; --max-time 30
// gives the daemon a chance to ask the user (cc's hook timeout default
// is 60s — we cap below to surface failures earlier).
//
// baseURL must be a loopback URL with no trailing slash (typically
// "http://127.0.0.1:NNNN" from HookServer.Addr()).
func SettingsForCC(baseURL string) ([]byte, error) {
	urls, err := HookURLs(baseURL)
	if err != nil {
		return nil, err
	}

	hooksMap := make(map[string]any, len(hookEvents))
	for _, ev := range hookEvents {
		url := urls[ev.name]
		hooksMap[ev.name] = []map[string]any{
			{
				"hooks": []map[string]any{
					{
						"type":    "command",
						"command": curlCommand(url),
					},
				},
			},
		}
	}

	doc := map[string]any{
		"hooks": hooksMap,
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hooks: marshal settings: %w", err)
	}
	return out, nil
}

// curlCommand renders the cc-canonical curl invocation that pipes its
// stdin into a POST to url and prints the response body. cc captures
// stdout as the hook reply.
//
//   - -s silences progress
//   - -f returns non-zero on HTTP 4xx/5xx (so cc treats the hook as
//     failed instead of trusting an error body)
//   - --max-time 30 caps wall time
//   - -X POST forces the method
//   - -H 'content-type: application/json' matches what the server expects
//   - --data-binary @- pipes stdin verbatim (no chunking, no transform)
func curlCommand(url string) string {
	return fmt.Sprintf(
		"curl -sf --max-time 30 -X POST -H 'content-type: application/json' --data-binary @- '%s'",
		url,
	)
}

// WriteSettingsFile generates SettingsForCC(baseURL) and writes it to
// dir/settings.json, returning the absolute path. File mode is 0600 —
// the URL is loopback-only but still treat as sensitive (port could be
// reused by an unrelated process). Errors propagate from filepath /
// SettingsForCC / os.WriteFile verbatim.
//
// **Overwrites silently** — the daemon owns the dir (typically
// `<workspace>/.tether/cc-settings/` per D-18) so prior settings.json
// content is expected to be a stale copy from a previous spawn. Caller
// MUST NOT pass a user-managed dir (e.g. `~/.claude/`); doing so will
// clobber the user's own cc settings.
func WriteSettingsFile(dir, baseURL string) (string, error) {
	body, err := SettingsForCC(baseURL)
	if err != nil {
		return "", err
	}
	if dir == "" {
		return "", errors.New("hooks: dir required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("hooks: abs dir: %w", err)
	}
	stat, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("hooks: stat dir: %w", err)
	}
	if !stat.IsDir() {
		return "", fmt.Errorf("hooks: %s is not a directory", abs)
	}
	path := filepath.Join(abs, "settings.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", fmt.Errorf("hooks: write settings: %w", err)
	}
	return path, nil
}
