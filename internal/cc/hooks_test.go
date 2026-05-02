package cc

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSettingsForCC_AllFiveHooksPresent(t *testing.T) {
	out, err := SettingsForCC("http://127.0.0.1:54321")
	if err != nil {
		t.Fatalf("SettingsForCC: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	hooks, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing hooks key: %s", out)
	}
	for _, ev := range []string{"PreToolUse", "PostToolUse", "SessionStart", "UserPromptSubmit", "Stop"} {
		if _, ok := hooks[ev]; !ok {
			t.Errorf("hooks.%s missing", ev)
		}
	}
}

func TestSettingsForCC_URLsContainBaseAndPath(t *testing.T) {
	const base = "http://127.0.0.1:99999"
	out, err := SettingsForCC(base)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, suffix := range []string{
		"/hooks/pre-tool-use",
		"/hooks/post-tool-use",
		"/hooks/session-start",
		"/hooks/user-prompt-submit",
		"/hooks/stop",
	} {
		want := base + suffix
		if !strings.Contains(s, want) {
			t.Errorf("settings.json missing URL %q", want)
		}
	}
}

func TestSettingsForCC_RejectsEmptyBaseURL(t *testing.T) {
	if _, err := SettingsForCC(""); err == nil {
		t.Fatal("SettingsForCC(\"\"): expected error")
	}
}

func TestSettingsForCC_RejectsTrailingSlash(t *testing.T) {
	// Trailing slash on baseURL would produce // in hook URL — reject up
	// front so the misconfiguration surfaces clearly.
	_, err := SettingsForCC("http://127.0.0.1:1234/")
	if err == nil {
		t.Fatal("trailing slash baseURL: expected error")
	}
	if !strings.Contains(err.Error(), "trailing slash") {
		t.Errorf("error should mention trailing slash: got %q", err.Error())
	}
}

func TestSettingsForCC_HookCommandsUseCurlPOST(t *testing.T) {
	out, err := SettingsForCC("http://127.0.0.1:54321")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("structure mismatch: %v", err)
	}
	for ev, entries := range doc.Hooks {
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Errorf("hooks.%s structure: %+v", ev, entries)
			continue
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" {
			t.Errorf("hooks.%s.type: got %q want command", ev, h.Type)
		}
		// curl POST with stdin pipe is the cc-canonical pattern.
		for _, mustContain := range []string{"curl", "-X", "POST", "@-"} {
			if !strings.Contains(h.Command, mustContain) {
				t.Errorf("hooks.%s.command missing %q: %q", ev, mustContain, h.Command)
			}
		}
	}
}

func TestWriteSettingsFile_WritesToDir(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteSettingsFile(dir, "http://127.0.0.1:54321")
	if err != nil {
		t.Fatalf("WriteSettingsFile: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %q not under dir %q", path, dir)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if !strings.Contains(string(body), "PreToolUse") {
		t.Errorf("settings file missing hook content: %s", body)
	}
	// File must be writeable but only by user (sensitive — contains URL).
	stat, _ := os.Stat(path)
	if stat.Mode().Perm()&0o077 != 0 {
		t.Errorf("settings file too permissive: %v", stat.Mode())
	}
}

func TestWriteSettingsFile_RejectsBadDir(t *testing.T) {
	if _, err := WriteSettingsFile("/nonexistent/path/that/does/not/exist", "http://x:1"); err == nil {
		t.Fatal("WriteSettingsFile bad dir: expected error")
	}
}

// --- end-to-end: settings → cc-shaped curl → hookserver round-trip --

// TestEndToEnd_SettingsRoundTripToHookServer uses HookURLs (review fix
// m12) instead of parsing the curl command string — much more robust to
// future format changes.
func TestEndToEnd_SettingsRoundTripToHookServer(t *testing.T) {
	h := &stubHandlers{preToolDecision: HookResponse{Decision: "allow", Reason: "round-trip"}}
	srv, _ := startTestServer(t, h)
	base := "http://" + srv.Addr()

	urls, err := HookURLs(base)
	if err != nil {
		t.Fatalf("HookURLs: %v", err)
	}
	preToolURL := urls["PreToolUse"]

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", preToolURL, strings.NewReader(`{"tool_name":"E2E"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("end-to-end POST: got %d", resp.StatusCode)
	}
	if len(h.preToolPayloads) != 1 {
		t.Errorf("handler not invoked: %d hits", len(h.preToolPayloads))
	}
}

// --- review fix: HookURLs API exposed for tests + non-loopback gate

func TestHookURLs_AllFiveEvents(t *testing.T) {
	urls, err := HookURLs("http://127.0.0.1:54321")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := map[string]string{
		"PreToolUse":       "/hooks/pre-tool-use",
		"PostToolUse":      "/hooks/post-tool-use",
		"SessionStart":     "/hooks/session-start",
		"UserPromptSubmit": "/hooks/user-prompt-submit",
		"Stop":             "/hooks/stop",
	}
	for ev, p := range wantPaths {
		got, ok := urls[ev]
		if !ok {
			t.Errorf("HookURLs missing %s", ev)
			continue
		}
		if got != "http://127.0.0.1:54321"+p {
			t.Errorf("HookURLs[%s]: got %q, want suffix %q", ev, got, p)
		}
	}
}

func TestHookURLs_RejectsNonLoopback(t *testing.T) {
	for _, baseURL := range []string{
		"http://example.com:8080",
		"http://192.168.1.5:8080",
		"https://malicious.example.org:443",
	} {
		if _, err := HookURLs(baseURL); err == nil {
			t.Errorf("HookURLs(%q): expected error, got nil", baseURL)
		}
	}
}

func TestHookURLs_AcceptsLoopbackVariants(t *testing.T) {
	for _, baseURL := range []string{
		"http://127.0.0.1:1",
		"http://localhost:9999",
		"http://[::1]:8080",
	} {
		if _, err := HookURLs(baseURL); err != nil {
			t.Errorf("HookURLs(%q): unexpected error %v", baseURL, err)
		}
	}
}

// --- review fix: WriteSettingsFile overwrite semantics is documented behavior

func TestWriteSettingsFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path1, err := WriteSettingsFile(dir, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	path2, err := WriteSettingsFile(dir, "http://127.0.0.1:2")
	if err != nil {
		t.Fatal(err)
	}
	if path1 != path2 {
		t.Errorf("path drift across calls: %q vs %q", path1, path2)
	}
	body, _ := os.ReadFile(path2)
	if !strings.Contains(string(body), "127.0.0.1:2") {
		t.Errorf("second WriteSettingsFile didn't overwrite: %s", body)
	}
	if strings.Contains(string(body), "127.0.0.1:1") {
		t.Errorf("first call's URL still present: %s", body)
	}
}
