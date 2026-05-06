package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/BurntSushi/toml"
)

// SkillNameRe is the canonical skill-name validator, shared with the
// fence-tag suffix grammar (spec §11.AA.1) and the install path naming
// rule (§11.Z.6). Kept here so manifest + CLI + adapter agree on one
// definition.
var SkillNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Manifest mirrors the v0.1 tether.toml schema (spec §11.Z.2). Every
// section except [skill] is optional; absence is a valid skill (degrades
// to a "plain cc plugin" — still installable, just no tether-specific
// hints).
type Manifest struct {
	Skill  SkillSection  `toml:"skill"`
	Tether TetherSection `toml:"tether"`
}

// SkillSection is the required identity block.
type SkillSection struct {
	Name        string      `toml:"name"`
	Version     string      `toml:"version"`
	Description string      `toml:"description"`
	Agents      AgentsBlock `toml:"agents"`
}

// AgentsBlock declares which agent backends a skill is authored for.
// v0.1 only honours `primary` for the cc adapter; `tested` is metadata.
type AgentsBlock struct {
	Primary string   `toml:"primary"`
	Tested  []string `toml:"tested"`
}

// TetherSection groups all tether-specific optional hints.
type TetherSection struct {
	FencedBlocks FencedBlocksBlock `toml:"fenced_blocks"`
	Mobile       MobileBlock       `toml:"mobile"`
	State        StateBlock        `toml:"state"`
}

// FencedBlocksBlock pre-registers the block kinds a skill emits, so the
// App can wire renderers eagerly (spec §11.Z.2 + §11.AA).
type FencedBlocksBlock struct {
	Emits           []string `toml:"emits"`
	PreferredLayout string   `toml:"preferred_layout"`
	MobileDefault   string   `toml:"mobile_default"`
}

// MobileBlock holds mobile-only UI hints (spec §11.Z.2).
type MobileBlock struct {
	DefaultCardLayout string   `toml:"default_card_layout"`
	QuickActions      []string `toml:"quick_actions"`
}

// StateBlock describes the skill's workspace state-file protocol
// (spec §11.Z.9). daemon never reads these files; the values here are
// enforced/used by skill scripts and `tether spawn`.
type StateBlock struct {
	WorkspaceDir     string `toml:"workspace_dir"`
	FileLock         bool   `toml:"file_lock"`
	GitignoreDefault bool   `toml:"gitignore_default"`
}

// WorkspaceManifest is the workspace-level tether.toml: who is enabled
// for this user workspace (spec §11.Z.6).
type WorkspaceManifest struct {
	Skills WorkspaceSkillsBlock `toml:"skills"`
}

// WorkspaceSkillsBlock holds the enabled-skill resolver result.
type WorkspaceSkillsBlock struct {
	Enabled []string `toml:"enabled"`
}

// LoadManifest reads + validates a per-skill tether.toml.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("skill: read manifest %s: %w", path, err)
	}
	var m Manifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("skill: parse manifest %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("skill: invalid manifest %s: %w", path, err)
	}
	return &m, nil
}

// Validate enforces the minimum field set for a v0.1 manifest.
func (m *Manifest) Validate() error {
	if m.Skill.Name == "" {
		return errors.New("[skill].name is required")
	}
	if !SkillNameRe.MatchString(m.Skill.Name) {
		return fmt.Errorf("[skill].name %q must match %s", m.Skill.Name, SkillNameRe.String())
	}
	if m.Skill.Version == "" {
		return errors.New("[skill].version is required")
	}
	if m.Skill.Agents.Primary == "" {
		return errors.New("[skill.agents].primary is required (v0.1: must be \"cc\")")
	}
	return nil
}

// LoadWorkspaceManifest reads the workspace tether.toml; missing file is
// treated as "no skills enabled" rather than an error.
func LoadWorkspaceManifest(workspaceDir string) (*WorkspaceManifest, error) {
	path := filepath.Join(workspaceDir, "tether.toml")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &WorkspaceManifest{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skill: read workspace manifest %s: %w", path, err)
	}
	var w WorkspaceManifest
	if err := toml.Unmarshal(b, &w); err != nil {
		return nil, fmt.Errorf("skill: parse workspace manifest %s: %w", path, err)
	}
	return &w, nil
}

// EnabledSkills is a thin accessor used by `tether spawn` and resolveSkills
// (B-6 fallback path; spec §11.Z.11).
func (w *WorkspaceManifest) EnabledSkills() []string {
	if w == nil {
		return nil
	}
	return w.Skills.Enabled
}
