package main

import (
	"os"
	"path/filepath"
)

// ccSettingsResolver resolves the cc settings.json path used to point a
// spawned `claude` subprocess at the daemon's hookserver. The seam exists
// because PR #44 wired the daemon's hookserver + settings.json writer, but
// the cc subprocess that ACTUALLY needs the file (anything spawning cc)
// must be told where it lives via `--settings <path>`.
//
// Precedence (highest first):
//
//  1. CLI flag (handled by callers; not by this resolver)
//  2. TETHER_HOOK_SETTINGS env var, when non-empty
//  3. <home>/.tether/cc-settings/settings.json on disk
//
// If nothing applies, returns empty string. An empty result MUST be
// interpreted by callers as "do not pass --settings to cc" — graceful
// fallback for users who run tether without `--auth-broker` (no daemon →
// no settings.json on disk).
//
// The resolver explicitly stats the default-path target so a stale env
// pointing at a missing file falls through to default-or-empty rather
// than producing an obviously-broken `--settings` arg. Conversely the
// env-var path is honored verbatim (no stat) — operators may legitimately
// pre-create the file out-of-band, and a noisy "your env var points
// nowhere" surface is better than silently dropping it.
//
// Pure-ish wrt environment: takes a homeProvider so tests can pin $HOME
// without leaking into the parent process's env.
func resolveCCSettings(homeProvider func() (string, error)) string {
	if v := os.Getenv("TETHER_HOOK_SETTINGS"); v != "" {
		return v
	}
	home, err := homeProvider()
	if err != nil || home == "" {
		return ""
	}
	def := filepath.Join(home, ".tether", "cc-settings", "settings.json")
	if _, err := os.Stat(def); err != nil {
		return ""
	}
	return def
}

// defaultHomeProvider is the production homeProvider used by resolveCCSettings.
func defaultHomeProvider() (string, error) {
	return os.UserHomeDir()
}
