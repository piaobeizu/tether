package doctor

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
)

// writeJSON writes v as JSON to path, creating parent dirs.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMCPSettingsInject_Injected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"type":                 "http",
				"url":                  "http://127.0.0.1:8899/mcp",
			},
		},
	})
	// Point home to temp dir by temporarily overriding UserHomeDir via env.
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(false)
	if !r.OK {
		t.Errorf("expected OK=true, message=%q", r.Message)
	}
}

func TestCheckMCPSettingsInject_Missing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPSettingsInject(false)
	if r.OK {
		t.Errorf("expected OK=false when settings.json absent, got message=%q", r.Message)
	}
}

func TestCheckMCPSettingsInject_NotInjected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"other": map[string]any{"url": "http://example.com"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPSettingsInject(false)
	if r.OK {
		t.Errorf("expected OK=false when no tether entry, got message=%q", r.Message)
	}
}

func TestCheckMCPAPITokens_NoFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := checkMCPAPITokens(false)
	if !r.OK {
		t.Errorf("missing api-tokens.json should not be an error: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_WithTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{
		"tokens": []map[string]any{
			{"id": "tok1", "name": "cursor", "hash": "abc"},
			{"id": "tok2", "name": "goose", "hash": "def"},
		},
	})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if !r.OK {
		t.Errorf("expected OK=true: %q", r.Message)
	}
	if r.Message != "2 external API token(s) configured" {
		t.Errorf("unexpected message: %q", r.Message)
	}
}

func TestCheckMCPAPITokens_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".tether", "api-tokens.json")
	writeJSON(t, path, map[string]any{"tokens": []any{}})
	t.Setenv("HOME", dir)
	r := checkMCPAPITokens(false)
	if !r.OK {
		t.Errorf("empty store should not be an error: %q", r.Message)
	}
}

func TestCheckMCPLoopback_NotRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No server is listening — expect a warning (OK=true, informational).
	r := checkMCPLoopback(false)
	if !r.OK {
		t.Errorf("unreachable loopback should be OK=true (warning), got: %q", r.Message)
	}
}

func TestCheckMCPLoopback_Running(t *testing.T) {
	// Start a throwaway TCP listener on an ephemeral port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	// Inject settings pointing at our listener.
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "settings.json")
	writeJSON(t, path, map[string]any{
		"mcpServers": map[string]any{
			"tether": map[string]any{
				agent.TetherManagedKey: true,
				"url": fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
			},
		},
	})
	t.Setenv("HOME", dir)

	r := checkMCPLoopback(false)
	if !r.OK {
		t.Errorf("expected OK=true with running listener: %q", r.Message)
	}
	if r.Message == "" || contains(r.Message, "not reachable") {
		t.Errorf("expected reachable message, got: %q", r.Message)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
