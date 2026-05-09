package server

import (
	"os"
	"os/exec"
	"path/filepath"
)

// resolveClaudePath finds the cc binary.
// Priority: TETHER_CC_PATH env → PATH lookup → well-known installer locations.
// Returns "claude" if nothing is found (exec will produce the original PATH error).
func resolveClaudePath() string {
	if env := os.Getenv("TETHER_CC_PATH"); env != "" {
		return env
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, candidate := range []string{
		filepath.Join(home, ".local/bin/claude"),
		filepath.Join(home, ".claude/local/bin/claude"),
		filepath.Join(home, ".npm-global/bin/claude"),
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
		"/usr/bin/claude",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return "claude"
}
