// Package skill implements the D-20 cc plugin overlay (symlink farm).
// Skills live in ~/.tether/skills/<id>/ (the canonical source).
// Enabling a skill for a workspace creates a symlink:
//   <workspacePath>/.claude/plugins/<id> → ~/.tether/skills/<id>/
// cc reads plugin manifests via the filesystem; symlinks mean updates to the
// skill source are seen by cc without an explicit copy (D-20 contract).
package skill

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Skill represents a plugin skill entry.
type Skill struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	SourcePath  string    `json:"sourcePath"` // ~/.tether/skills/<id>/
	AddedAt     time.Time `json:"addedAt"`
}

// Registry manages installed skills and their workspace symlinks.
type Registry struct {
	mu     sync.RWMutex
	skills []Skill
	path   string // ~/.tether/skills.json
}

// NewRegistry loads (or creates) the skill registry.
func NewRegistry() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether/skills: %w", err)
	}
	path := filepath.Join(dir, "skills.json")
	r := &Registry{path: path}
	_ = r.load()
	return r, nil
}

// List returns all installed skills.
func (r *Registry) List() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, len(r.skills))
	copy(out, r.skills)
	return out
}

// Install registers a skill from sourcePath and assigns an ID.
func (r *Registry) Install(name, sourcePath string) (Skill, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return Skill{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return Skill{}, fmt.Errorf("skill path not found: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.skills {
		if s.SourcePath == abs {
			return s, nil
		}
	}
	s := Skill{
		ID:         newSkillID(),
		Name:       name,
		SourcePath: abs,
		AddedAt:    time.Now().UTC(),
	}
	r.skills = append(r.skills, s)
	return s, r.saveLocked()
}

// Remove uninstalls a skill by ID (does NOT remove workspace symlinks).
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.skills {
		if s.ID != id {
			r.skills[n] = s
			n++
		}
	}
	r.skills = r.skills[:n]
	return r.saveLocked()
}

// Enable creates a symlink for skillID in the given workspacePath (D-20 §3).
// <workspacePath>/.claude/plugins/<skillID> → skill.SourcePath
func (r *Registry) Enable(skillID, workspacePath string) error {
	r.mu.RLock()
	var sk *Skill
	for i := range r.skills {
		if r.skills[i].ID == skillID {
			sk = &r.skills[i]
			break
		}
	}
	r.mu.RUnlock()
	if sk == nil {
		return fmt.Errorf("skill %q not found", skillID)
	}
	pluginsDir := filepath.Join(workspacePath, ".claude", "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir plugins: %w", err)
	}
	link := filepath.Join(pluginsDir, skillID)
	_ = os.Remove(link) // remove stale link
	return os.Symlink(sk.SourcePath, link)
}

// Disable removes the symlink for skillID from workspacePath.
func (r *Registry) Disable(skillID, workspacePath string) error {
	link := filepath.Join(workspacePath, ".claude", "plugins", skillID)
	err := os.Remove(link)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &r.skills)
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.skills, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func newSkillID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
