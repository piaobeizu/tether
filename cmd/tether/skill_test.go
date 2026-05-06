package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestSkillListJSON_EmptyPool — exercises the `--json` path against
// an empty pool. The contract for tether-app's Tauri bridge is that
// stdout is ALWAYS valid JSON (even when the pool is empty), so the
// frontend can parse without sniffing the exit code.
func TestSkillListJSON_EmptyPool(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	// pool root = $HOME/.tether/skills — left empty / nonexistent.

	read, restore := captureStdout(t)
	rc := skillList([]string{"--json"})
	stdout := read()
	restore()
	if rc != 0 {
		t.Fatalf("skillList(--json) returned %d, want 0", rc)
	}

	var rows []skillListJSONRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v (raw=%q)", err, stdout)
	}
	if len(rows) != 0 {
		t.Errorf("expected empty array, got %+v", rows)
	}
}

// TestSkillListJSON_PopulatedPool — drops two skill directories into
// the pool (one with a tether.toml, one without) and asserts the JSON
// shape. The tether-app TS-side `loadSkills.mapSkill` translation is
// independently unit-tested in src/store/loadSkills.test.ts.
func TestSkillListJSON_PopulatedPool(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	pool := filepath.Join(tmp, ".tether", "skills")
	if err := os.MkdirAll(filepath.Join(pool, "refactor-code"), 0o755); err != nil {
		t.Fatalf("setup pool dir: %v", err)
	}
	manifest := `[skill]
name = "refactor-code"
version = "0.4.2"
description = "DAG-driven code restructuring"

[skill.agents]
primary = "cc"
`
	if err := os.WriteFile(filepath.Join(pool, "refactor-code", "tether.toml"),
		[]byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	// Plain plugin (no tether.toml) — should still appear in the
	// listing with an empty version.
	if err := os.MkdirAll(filepath.Join(pool, "plain-plugin"), 0o755); err != nil {
		t.Fatalf("setup plain plugin: %v", err)
	}

	read, restore := captureStdout(t)
	rc := skillList([]string{"--json"})
	stdout := read()
	restore()
	if rc != 0 {
		t.Fatalf("skillList(--json) returned %d, want 0", rc)
	}

	var rows []skillListJSONRow
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v (raw=%q)", err, stdout)
	}
	byName := map[string]skillListJSONRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	if got := byName["refactor-code"]; got.Version != "0.4.2" ||
		got.Description != "DAG-driven code restructuring" {
		t.Errorf("refactor-code row mismatch: %+v", got)
	}
	if got, ok := byName["plain-plugin"]; !ok {
		t.Errorf("plain-plugin missing from listing")
	} else if got.Version != "" {
		t.Errorf("plain-plugin should have empty version, got %q", got.Version)
	}
}

// TestSkillListJSON_RejectsExtraArgs — flag parsing should reject
// positional args after `--json` (matches the non-JSON path's strict
// arity check).
func TestSkillListJSON_RejectsExtraArgs(t *testing.T) {
	if rc := skillList([]string{"--json", "extra"}); rc != 2 {
		t.Errorf("skillList(--json extra) = %d, want 2", rc)
	}
}

// captureStdout helper lives in resume_test.go (returns read+restore
// closures). We reuse it here.
