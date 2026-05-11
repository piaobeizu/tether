package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInjectAndRemoveMCPServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}

	const port = 8899
	const token = "testtoken123"

	if err := InjectMCPServer(port, token); err != nil {
		t.Fatalf("InjectMCPServer: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	mcpServers, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers not found")
	}
	tether, ok := mcpServers["tether"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers.tether not found")
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	if tether["url"] != wantURL {
		t.Errorf("url = %q, want %q", tether["url"], wantURL)
	}
	headers, _ := tether["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer "+token {
		t.Errorf("Authorization = %q, want Bearer %s", headers["Authorization"], token)
	}

	// Idempotent re-inject — must not duplicate.
	if err := InjectMCPServer(port, token); err != nil {
		t.Fatalf("second InjectMCPServer: %v", err)
	}
	data2, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	var settings2 map[string]any
	json.Unmarshal(data2, &settings2)
	mc2 := settings2["mcpServers"].(map[string]any)
	if len(mc2) != 1 {
		t.Errorf("expected 1 mcpServers entry after re-inject, got %d", len(mc2))
	}

	// Remove.
	if err := RemoveMCPServer(); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	data3, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	var settings3 map[string]any
	json.Unmarshal(data3, &settings3)
	if _, has := settings3["mcpServers"]; has {
		t.Error("mcpServers should be absent after Remove")
	}
}
