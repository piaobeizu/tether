package server

import (
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveClaudePath finds the cc binary.
// Priority: TETHER_CC_PATH env → PATH lookup → well-known installer locations.
// Returns "claude" if nothing is found (exec will produce the original PATH error).
//
// Exported for `tether doctor`, which reported "not found on PATH" for a host
// this function resolves fine — TETHER_CC_PATH set, or an installer directory
// that is not on the PATH of whoever runs doctor. A diagnostic that answers a
// narrower question than the spawn it predicts is a false alarm generator
// (tether#84).
func ResolveClaudePath() string {
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
