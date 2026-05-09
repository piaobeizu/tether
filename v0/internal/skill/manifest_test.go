package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadManifest_HappyPath(t *testing.T) {
	const src = `
[skill]
name = "dag"
version = "0.1.0"
description = "DAG-driven multi-step planning + execution skill"

[skill.agents]
primary = "cc"
tested = ["cc"]

[tether.fenced_blocks]
emits = ["dag", "media"]
preferred_layout = "full"
mobile_default = "compact"

[tether.mobile]
default_card_layout = "compact"
quick_actions = ["rollback", "approve"]

[tether.state]
workspace_dir = ".tether/dag/"
file_lock = true
gitignore_default = true
`
	path := writeTmp(t, "tether.toml", src)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Skill.Name != "dag" {
		t.Errorf("name: got %q want dag", m.Skill.Name)
	}
	if m.Skill.Version != "0.1.0" {
		t.Errorf("version: got %q", m.Skill.Version)
	}
	if m.Skill.Agents.Primary != "cc" {
		t.Errorf("agents.primary: got %q", m.Skill.Agents.Primary)
	}
	if got := m.Tether.FencedBlocks.Emits; len(got) != 2 || got[0] != "dag" || got[1] != "media" {
		t.Errorf("fenced_blocks.emits: got %v", got)
	}
	if !m.Tether.State.FileLock || !m.Tether.State.GitignoreDefault {
		t.Errorf("tether.state booleans not parsed: %+v", m.Tether.State)
	}
	if m.Tether.State.WorkspaceDir != ".tether/dag/" {
		t.Errorf("workspace_dir: got %q", m.Tether.State.WorkspaceDir)
	}
	if got := m.Tether.Mobile.QuickActions; len(got) != 2 {
		t.Errorf("quick_actions: %v", got)
	}
}

func TestLoadManifest_MinimalValid(t *testing.T) {
	// Only required fields; everything else absent.
	const src = `
[skill]
name = "foo"
version = "1.0.0"

[skill.agents]
primary = "cc"
`
	path := writeTmp(t, "tether.toml", src)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Skill.Name != "foo" {
		t.Errorf("name: got %q", m.Skill.Name)
	}
	if len(m.Tether.FencedBlocks.Emits) != 0 {
		t.Errorf("expected no emits, got %v", m.Tether.FencedBlocks.Emits)
	}
}

func TestLoadManifest_Malformed(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		wantSub string
	}{
		{
			name:    "missing name",
			src:     "[skill]\nversion = \"1\"\n[skill.agents]\nprimary = \"cc\"\n",
			wantSub: "[skill].name is required",
		},
		{
			name:    "missing version",
			src:     "[skill]\nname = \"foo\"\n[skill.agents]\nprimary = \"cc\"\n",
			wantSub: "[skill].version is required",
		},
		{
			name:    "missing primary agent",
			src:     "[skill]\nname = \"foo\"\nversion = \"1\"\n",
			wantSub: "[skill.agents].primary is required",
		},
		{
			name:    "invalid name chars",
			src:     "[skill]\nname = \"bad name\"\nversion = \"1\"\n[skill.agents]\nprimary = \"cc\"\n",
			wantSub: "must match",
		},
		{
			name:    "broken toml syntax",
			src:     "[skill]\nname =\n",
			wantSub: "parse manifest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTmp(t, "tether.toml", tc.src)
			_, err := LoadManifest(path)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q missing substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestLoadWorkspaceManifest_AbsentMeansNoSkills(t *testing.T) {
	dir := t.TempDir()
	w, err := LoadWorkspaceManifest(dir)
	if err != nil {
		t.Fatalf("LoadWorkspaceManifest: %v", err)
	}
	if got := w.EnabledSkills(); len(got) != 0 {
		t.Errorf("expected empty enabled, got %v", got)
	}
}

func TestLoadWorkspaceManifest_HappyPath(t *testing.T) {
	dir := t.TempDir()
	src := "[skills]\nenabled = [\"dag\", \"writing-plans\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "tether.toml"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := LoadWorkspaceManifest(dir)
	if err != nil {
		t.Fatalf("LoadWorkspaceManifest: %v", err)
	}
	got := w.EnabledSkills()
	if len(got) != 2 || got[0] != "dag" || got[1] != "writing-plans" {
		t.Errorf("enabled: got %v", got)
	}
}

// --- helpers --------------------------------------------------------

func writeTmp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
