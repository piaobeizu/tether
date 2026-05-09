package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// TetherManagedKey is the sentinel field injected into tether-owned hook entries.
const TetherManagedKey = "_tether_managed"

// InjectPermHook merges the tether-managed PreToolUse hook entry into
// ~/.config/claude/settings.json, preserving any existing user hooks.
// Uses atomic rename to avoid partial writes (D-05b §5.1, §10 row 1).
func InjectPermHook(hookBinPath, daemonEndpoint string) error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}

	settings, err := loadSettings(path)
	if err != nil {
		settings = map[string]any{}
	}

	// Remove any stale tether-managed entries before adding fresh one.
	removeManaged(settings)

	hooks := getHookList(settings, "PreToolUse")
	hooks = append(hooks, map[string]any{
		TetherManagedKey: true,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": hookBinPath,
		}},
		"matcher": "*",
	})
	setHookList(settings, "PreToolUse", hooks)

	return saveSettings(path, settings)
}

// RemovePermHook removes all tether-managed PreToolUse entries from settings.json.
// Called on graceful shutdown (D-05b §5.2).
func RemovePermHook() error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}
	settings, err := loadSettings(path)
	if err != nil {
		return nil // file absent = nothing to clean up
	}
	removeManaged(settings)
	return saveSettings(path, settings)
}

func removeManaged(settings map[string]any) {
	for _, kind := range []string{"PreToolUse", "PostToolUse"} {
		hooks := getHookList(settings, kind)
		filtered := hooks[:0]
		for _, h := range hooks {
			if hm, ok := h.(map[string]any); ok {
				if managed, _ := hm[TetherManagedKey].(bool); managed {
					continue
				}
			}
			filtered = append(filtered, h)
		}
		if len(filtered) == 0 {
			deleteHookList(settings, kind)
		} else {
			setHookList(settings, kind, filtered)
		}
	}
}

func ccSettingsPath() (string, error) {
	// cc reads ~/.config/claude/settings.json on Linux/Mac.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

func loadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

func saveSettings(path string, settings map[string]any) error {
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write settings tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func getHookList(settings map[string]any, kind string) []any {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	list, _ := hooks[kind].([]any)
	return list
}

func setHookList(settings map[string]any, kind string, list []any) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	hooks[kind] = list
}

func deleteHookList(settings map[string]any, kind string) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks != nil {
		delete(hooks, kind)
	}
}
